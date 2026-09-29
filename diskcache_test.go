package main

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testChunk = 64 << 10

// smallChunks makes the disk cache use 64 KiB chunks and no eviction slack
// for the duration of a test.
func smallChunks(t *testing.T) {
	t.Helper()
	oldChunk, oldSlack := diskChunkSize, evictSlackBytes
	diskChunkSize, evictSlackBytes = testChunk, 0
	t.Cleanup(func() { diskChunkSize, evictSlackBytes = oldChunk, oldSlack })
}

// writeImage creates an "image" file of n bytes of seeded random data.
func writeImage(t *testing.T, dir, name string, n int, seed uint64) (string, []byte) {
	t.Helper()
	data := make([]byte, n)
	rand.NewChaCha8([32]byte{byte(seed)}).Read(data)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

// countingSource wraps an image's source reader.
type countingSource struct {
	r       io.ReaderAt
	reads   atomic.Int64
	delay   time.Duration
	offline atomic.Bool
}

func (c *countingSource) ReadAt(p []byte, off int64) (int, error) {
	if c.offline.Load() {
		return 0, errors.New("share unreachable")
	}
	c.reads.Add(1)
	time.Sleep(c.delay)
	return c.r.ReadAt(p, off)
}

func openCounted(t *testing.T, dc *DiskCache, path string) (*CachedImage, *countingSource) {
	t.Helper()
	img, err := dc.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	src := &countingSource{r: img.src}
	img.src = src
	t.Cleanup(func() { img.Close() })
	return img, src
}

func newTestCache(t *testing.T, maxBytes, minFree int64) *DiskCache {
	t.Helper()
	dc, err := NewDiskCache(filepath.Join(t.TempDir(), "cache"), maxBytes, minFree)
	if err != nil {
		t.Fatal(err)
	}
	return dc
}

func readAll(t *testing.T, r io.ReaderAt, size int) []byte {
	t.Helper()
	buf := make([]byte, size)
	if n, err := r.ReadAt(buf, 0); n != size || (err != nil && err != io.EOF) {
		t.Fatalf("ReadAt(all) = %d, %v", n, err)
	}
	return buf
}

func TestDiskCacheReadThrough(t *testing.T) {
	smallChunks(t)
	imgPath, want := writeImage(t, t.TempDir(), "a.sqfs", 10*testChunk+123, 1)
	img, src := openCounted(t, newTestCache(t, 0, 0), imgPath)

	rng := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 300; i++ {
		off := rng.IntN(len(want) + 10)
		n := 1 + rng.IntN(3*testChunk)
		buf := make([]byte, n)
		got, err := img.ReadAt(buf, int64(off))
		wantN := max(0, min(n, len(want)-off))
		if got != wantN || (wantN < n && err != io.EOF) || (wantN == n && err != nil) {
			t.Fatalf("ReadAt(off=%d, n=%d) = %d, %v; want %d", off, n, got, err, wantN)
		}
		if !bytes.Equal(buf[:got], want[off:off+got]) {
			t.Fatalf("ReadAt(off=%d, n=%d): content mismatch", off, n)
		}
	}
	if r := src.reads.Load(); r > 11 {
		t.Errorf("source reads = %d, want at most one per chunk (11)", r)
	}
	before := src.reads.Load()
	readAll(t, img, len(want))
	if extra := src.reads.Load() - before; extra > 11-before {
		t.Errorf("full read after random reads fetched %d chunks again", extra)
	}
}

func TestDiskCachePersistsAcrossOpen(t *testing.T) {
	smallChunks(t)
	imgPath, want := writeImage(t, t.TempDir(), "a.sqfs", 5*testChunk, 1)
	dir := filepath.Join(t.TempDir(), "cache")

	dc1, _ := NewDiskCache(dir, 0, 0)
	img1, err := dc1.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, img1, len(want))
	img1.Close()

	dc2, _ := NewDiskCache(dir, 0, 0)
	img2, src := openCounted(t, dc2, imgPath)
	if got := readAll(t, img2, len(want)); !bytes.Equal(got, want) {
		t.Fatal("content mismatch after reopen")
	}
	if r := src.reads.Load(); r != 0 {
		t.Errorf("source reads after reopen = %d, want 0", r)
	}
}

func TestDiskCacheRefetchesCorruptChunk(t *testing.T) {
	smallChunks(t)
	imgPath, want := writeImage(t, t.TempDir(), "a.sqfs", 4*testChunk, 1)
	dir := filepath.Join(t.TempDir(), "cache")
	dc, _ := NewDiskCache(dir, 0, 0)
	img, _ := dc.Open(imgPath)
	readAll(t, img, len(want))
	id := img.id
	img.Close()

	// Flip bytes in chunk 2 of the cached data, as a torn write would.
	data, err := os.OpenFile(filepath.Join(dir, id+".data"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	data.WriteAt([]byte("garbage"), 2*testChunk+100)
	data.Close()

	img2, src := openCounted(t, dc, imgPath)
	if got := readAll(t, img2, len(want)); !bytes.Equal(got, want) {
		t.Fatal("served corrupt data")
	}
	if r := src.reads.Load(); r != 1 {
		t.Errorf("source reads = %d, want 1 (only the corrupt chunk)", r)
	}
}

func TestDiskCacheDropsCacheOfChangedImage(t *testing.T) {
	smallChunks(t)
	dir := t.TempDir()
	imgPath, _ := writeImage(t, dir, "a.sqfs", 3*testChunk, 1)
	dc := newTestCache(t, 0, 0)
	img, _ := dc.Open(imgPath)
	readAll(t, img, 3*testChunk)
	oldID := img.id
	img.Close()

	time.Sleep(20 * time.Millisecond) // make sure the mtime moves
	_, want := writeImage(t, dir, "a.sqfs", 3*testChunk, 2)
	img2, _ := openCounted(t, dc, imgPath)
	if img2.id == oldID {
		t.Fatal("changed image got the same cache id")
	}
	if got := readAll(t, img2, len(want)); !bytes.Equal(got, want) {
		t.Fatal("served data from the old image")
	}
	if _, err := os.Stat(filepath.Join(dc.dir, oldID+".data")); !os.IsNotExist(err) {
		t.Errorf("old cache not removed: %v", err)
	}
}

func TestDiskCacheServesUnreachableImage(t *testing.T) {
	smallChunks(t)
	dir := t.TempDir()
	imgPath, want := writeImage(t, dir, "a.sqfs", 4*testChunk, 1)
	dc := newTestCache(t, 0, 0)
	img, _ := dc.Open(imgPath)
	buf := make([]byte, 2*testChunk) // cache chunks 0 and 1 only
	img.ReadAt(buf, 0)
	img.Close()

	if err := os.Rename(imgPath, imgPath+".away"); err != nil {
		t.Fatal(err)
	}
	img2, err := dc.Open(imgPath)
	if err != nil {
		t.Fatalf("Open with image unreachable: %v", err)
	}
	defer img2.Close()
	if img2.Online() {
		t.Error("Online() = true for an unreachable image")
	}
	if img2.Size() != int64(len(want)) {
		t.Errorf("Size = %d, want %d", img2.Size(), len(want))
	}
	if _, err := img2.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, want[:2*testChunk]) {
		t.Errorf("cached chunks: err=%v match=%v", err, bytes.Equal(buf, want[:2*testChunk]))
	}
	if _, err := img2.ReadAt(buf[:10], 3*testChunk); !errors.Is(err, errChunkAbsent) {
		t.Errorf("uncached chunk err = %v, want errChunkAbsent", err)
	}
}

// fakeClock returns a settable clock for the cache's access times.
func fakeClock(dc *DiskCache) *time.Time {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dc.now = func() time.Time { return now }
	return &now
}

func present(img *CachedImage, i int) bool {
	img.mu.Lock()
	defer img.mu.Unlock()
	return img.entries[i].atime != 0
}

func TestDiskCacheEvictsLeastRecentlyUsedAcrossImages(t *testing.T) {
	smallChunks(t)
	dir := t.TempDir()
	aPath, aWant := writeImage(t, dir, "a.sqfs", 4*testChunk, 1)
	bPath, _ := writeImage(t, dir, "b.sqfs", 4*testChunk, 2)
	dc := newTestCache(t, 3*testChunk, 0)
	clock := fakeClock(dc)
	a, _ := openCounted(t, dc, aPath)
	b, _ := openCounted(t, dc, bPath)
	buf := make([]byte, 10)
	at := func(img *CachedImage, chunk int, minute int) {
		*clock = clock.Add(time.Duration(minute) * time.Minute)
		if _, err := img.ReadAt(buf, int64(chunk)*testChunk); err != nil {
			t.Fatal(err)
		}
	}
	at(a, 0, 1)
	at(a, 1, 1)
	at(b, 0, 1)
	at(a, 0, 1) // a/0 is now newer than a/1
	at(b, 1, 1) // fourth chunk: the oldest, a/1, must go

	for _, c := range []struct {
		img  *CachedImage
		i    int
		want bool
	}{{a, 0, true}, {a, 1, false}, {b, 0, true}, {b, 1, true}} {
		if got := present(c.img, c.i); got != c.want {
			t.Errorf("%s chunk %d cached = %v, want %v", filepath.Base(c.img.hdr.path), c.i, got, c.want)
		}
	}
	dc.mu.Lock()
	u := dc.usage()
	dc.mu.Unlock()
	if u > 3*testChunk {
		t.Errorf("usage = %d, over the cap %d", u, 3*testChunk)
	}
	// An evicted chunk is fetched again and still correct.
	got := make([]byte, testChunk)
	if _, err := a.ReadAt(got, testChunk); err != nil || !bytes.Equal(got, aWant[testChunk:2*testChunk]) {
		t.Errorf("re-read of evicted chunk: err=%v match=%v", err, bytes.Equal(got, aWant[testChunk:2*testChunk]))
	}
}

func TestDiskCacheEvictsImagesNotInUse(t *testing.T) {
	smallChunks(t)
	dir := t.TempDir()
	aPath, _ := writeImage(t, dir, "a.sqfs", 3*testChunk, 1)
	bPath, _ := writeImage(t, dir, "b.sqfs", 3*testChunk, 2)
	dc := newTestCache(t, 3*testChunk, 0)
	clock := fakeClock(dc)

	a, _ := dc.Open(aPath)
	readAll(t, a, 3*testChunk)
	aID := a.id
	a.Close()

	*clock = clock.Add(time.Hour)
	b, _ := openCounted(t, dc, bPath)
	readAll(t, b, 3*testChunk)

	if _, err := os.Stat(filepath.Join(dc.dir, aID+".idx")); !os.IsNotExist(err) {
		t.Errorf("fully evicted cache of a closed image was not deleted (stat err %v)", err)
	}
	for i := 0; i < 3; i++ {
		if !present(b, i) {
			t.Errorf("b chunk %d evicted instead of the older, closed image", i)
		}
	}
}

func TestDiskCacheKeepsFreeSpaceFloor(t *testing.T) {
	smallChunks(t)
	imgPath, _ := writeImage(t, t.TempDir(), "a.sqfs", 8*testChunk, 1)
	dc := newTestCache(t, 0, 10*testChunk)
	fakeClock(dc)
	// The disk has room for 13 chunks; the floor keeps 10 of them free.
	dc.freeSpace = func(string) (int64, error) { return 13*testChunk - dc.usage(), nil }
	img, _ := openCounted(t, dc, imgPath)
	readAll(t, img, 8*testChunk)

	dc.mu.Lock()
	u := dc.usage()
	dc.mu.Unlock()
	if u > 3*testChunk {
		t.Errorf("usage = %d chunks, want at most 3 to keep the free-space floor", u/testChunk)
	}
}

func TestDiskCacheSharesConcurrentFetch(t *testing.T) {
	smallChunks(t)
	imgPath, want := writeImage(t, t.TempDir(), "a.sqfs", 2*testChunk, 1)
	img, src := openCounted(t, newTestCache(t, 0, 0), imgPath)
	src.delay = 30 * time.Millisecond

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 100)
			if _, err := img.ReadAt(buf, 50); err != nil || !bytes.Equal(buf, want[50:150]) {
				t.Errorf("ReadAt: err=%v match=%v", err, bytes.Equal(buf, want[50:150]))
			}
		}()
	}
	wg.Wait()
	if r := src.reads.Load(); r != 1 {
		t.Errorf("source reads = %d, want 1", r)
	}
}

// Readers hammer a cache that can hold two chunks, so fetches and evictions
// constantly interleave. Every read must still return the image's bytes.
func TestDiskCacheConcurrentReadsWithEviction(t *testing.T) {
	smallChunks(t)
	imgPath, want := writeImage(t, t.TempDir(), "a.sqfs", 6*testChunk, 1)
	img, _ := openCounted(t, newTestCache(t, 2*testChunk, 0), imgPath)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, 9))
			for i := 0; i < 200; i++ {
				off := rng.IntN(len(want) - 4096)
				buf := make([]byte, 4096)
				if _, err := img.ReadAt(buf, int64(off)); err != nil {
					t.Errorf("ReadAt(%d): %v", off, err)
					return
				}
				if !bytes.Equal(buf, want[off:off+4096]) {
					t.Errorf("ReadAt(%d): content mismatch", off)
					return
				}
			}
		}(uint64(g))
	}
	wg.Wait()
}

func TestDiskCacheBusyFallsBackToDirectReads(t *testing.T) {
	smallChunks(t)
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "fixture.sqfs")
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(imgPath, b, 0o644)
	cacheDir := filepath.Join(dir, "cache")

	dc1, _ := NewDiskCache(cacheDir, 0, 0)
	holder, err := dc1.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	dc2, _ := NewDiskCache(cacheDir, 0, 0)
	if _, err := dc2.Open(imgPath); !errors.Is(err, errCacheBusy) {
		t.Fatalf("second Open err = %v, want errCacheBusy", err)
	}
	sq, img, err := NewCachedSquashLayer(imgPath, dc2)
	if err != nil || img != nil || sq == nil {
		t.Fatalf("NewCachedSquashLayer = %v, %v, %v; want a direct layer", sq, img, err)
	}
	sq.Close()
}

func TestDiskCachePrefetch(t *testing.T) {
	smallChunks(t)
	imgPath, want := writeImage(t, t.TempDir(), "a.sqfs", 6*testChunk, 1)

	t.Run("fits", func(t *testing.T) {
		img, src := openCounted(t, newTestCache(t, 0, 0), imgPath)
		if n, err := img.Prefetch(); n != 6 || err != nil {
			t.Fatalf("Prefetch = %d, %v; want 6, nil", n, err)
		}
		before := src.reads.Load()
		if got := readAll(t, img, len(want)); !bytes.Equal(got, want) {
			t.Fatal("content mismatch")
		}
		if src.reads.Load() != before {
			t.Error("reads after a full prefetch went to the source")
		}
	})
	t.Run("stops at the cap without evicting", func(t *testing.T) {
		dc := newTestCache(t, 4*testChunk, 0)
		img, _ := openCounted(t, dc, imgPath)
		if n, err := img.Prefetch(); n != 4 || err != nil {
			t.Fatalf("Prefetch = %d, %v; want 4, nil", n, err)
		}
		for i := 0; i < 4; i++ {
			if !present(img, i) {
				t.Errorf("chunk %d missing: prefetch evicted its own chunks", i)
			}
		}
	})
}

// The squashfs layer reads correctly through the disk cache, and a second
// mount serves everything from it.
func TestSquashLayerThroughDiskCache(t *testing.T) {
	smallChunks(t)
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "fixture.sqfs")
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(imgPath, b, 0o644)
	dc := newTestCache(t, 0, 0)

	for pass := 0; pass < 2; pass++ {
		sq, img, err := NewCachedSquashLayer(imgPath, dc)
		if err != nil || img == nil {
			t.Fatalf("pass %d: NewCachedSquashLayer: %v (img %v)", pass, err, img)
		}
		src := &countingSource{r: img.src}
		img.src = src
		f, err := sq.Open("/big.bin")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(f.(io.Reader))
		f.Close()
		if err != nil || !bytes.Equal(got, fixtureBig()) {
			t.Fatalf("pass %d: big.bin err=%v match=%v", pass, err, bytes.Equal(got, fixtureBig()))
		}
		if pass == 1 && src.reads.Load() != 0 {
			t.Errorf("second mount read %d chunks from the image", src.reads.Load())
		}
		img.Close()
	}
}
