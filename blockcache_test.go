package main

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openFixtureCache(t testing.TB, maxBytes int64) *SquashLayer {
	t.Helper()
	sq, err := NewSquashLayer(fixturePath)
	if err != nil {
		t.Fatalf("NewSquashLayer(%s): %v", fixturePath, err)
	}
	sq.SetCacheSize(maxBytes)
	return sq
}

func readerAt(t testing.TB, sq *SquashLayer, name string) io.ReaderAt {
	t.Helper()
	f, err := sq.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f.(io.ReaderAt)
}

func TestOpenUsesCacheOnlyWhenEnabled(t *testing.T) {
	if _, ok := readerAt(t, openFixtureCache(t, defaultCacheBytes), "/big.bin").(*cachedFile); !ok {
		t.Error("cache enabled: Open did not return a *cachedFile")
	}
	if _, ok := readerAt(t, openFixtureCache(t, 0), "/big.bin").(*cachedFile); ok {
		t.Error("cache disabled: Open returned a *cachedFile")
	}
}

// Random reads through the cache must equal the fixture content, for a cache
// large enough to hold the file and for one that holds a single block (every
// other access evicts).
func TestCachedReadAtMatchesContent(t *testing.T) {
	want := fixtureBig()
	for _, tc := range []struct {
		name     string
		maxBytes int64
	}{
		{"large", 64 << 20},
		{"one-block", fixtureBlockSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ra := readerAt(t, openFixtureCache(t, tc.maxBytes), "/big.bin")
			rng := rand.New(rand.NewPCG(1, 2))
			size := len(want)
			for i := 0; i < 500; i++ {
				off := rng.IntN(size + 10) // occasionally past EOF
				n := 1 + rng.IntN(3*fixtureBlockSize/2)
				buf := make([]byte, n)
				got, err := ra.ReadAt(buf, int64(off))

				wantN := max(0, min(n, size-off))
				switch {
				case got != wantN:
					t.Fatalf("ReadAt(off=%d, n=%d) = %d bytes, want %d", off, n, got, wantN)
				case wantN < n && err != io.EOF:
					t.Fatalf("ReadAt(off=%d, n=%d) short read err = %v, want io.EOF", off, n, err)
				case wantN == n && err != nil:
					t.Fatalf("ReadAt(off=%d, n=%d) err = %v", off, n, err)
				case !bytes.Equal(buf[:got], want[off:off+got]):
					t.Fatalf("ReadAt(off=%d, n=%d): content mismatch", off, n)
				}
			}
		})
	}
}

// Sequential small reads decompress each block once.
func TestCachedSequentialReadsDecompressEachBlockOnce(t *testing.T) {
	sq := openFixtureCache(t, 64<<20)
	f, err := sq.Open("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(struct{ io.Reader }{f}) // plain Read, 512-byte-ish chunks
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fixtureBig()) {
		t.Fatal("content mismatch")
	}
	blocks := int64((len(got) + fixtureBlockSize - 1) / fixtureBlockSize)
	if m := sq.cache.misses.Load(); m != blocks {
		t.Errorf("misses = %d, want one per block (%d)", m, blocks)
	}
}

func TestBlockCacheEvictsLRUWithinBound(t *testing.T) {
	c := newBlockCache(3 * 10)
	load := func(k int64) {
		c.get(blockKey{block: k}, func() ([]byte, error) { return make([]byte, 10), nil })
	}
	for k := int64(0); k < 3; k++ {
		load(k)
	}
	load(0) // 0 becomes most recent; 1 is now least recent
	load(3) // evicts 1
	if c.bytes > c.maxBytes {
		t.Fatalf("bytes = %d > max %d", c.bytes, c.maxBytes)
	}
	for k, want := range map[int64]bool{0: true, 1: false, 2: true, 3: true} {
		if _, ok := c.entries[blockKey{block: k}]; ok != want {
			t.Errorf("block %d cached = %v, want %v", k, ok, want)
		}
	}
}

func TestBlockCacheSharesConcurrentLoad(t *testing.T) {
	c := newBlockCache(1 << 20)
	var loads atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.get(blockKey{ino: 7}, func() ([]byte, error) {
				loads.Add(1)
				<-release
				return []byte("block"), nil
			})
			if err != nil || string(data) != "block" {
				t.Errorf("get = %q, %v", data, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // let every goroutine reach get
	close(release)
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Errorf("loader ran %d times, want 1", n)
	}
}

func TestBlockCacheDoesNotCacheErrors(t *testing.T) {
	c := newBlockCache(1 << 20)
	boom := errors.New("boom")
	if _, err := c.get(blockKey{}, func() ([]byte, error) { return nil, boom }); err != boom {
		t.Fatalf("err = %v, want boom", err)
	}
	data, err := c.get(blockKey{}, func() ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(data) != "ok" {
		t.Fatalf("after error: get = %q, %v; want retry to load", data, err)
	}
}

// 4 KiB sequential reads, the typical WinFsp/FUSE request size.
//
//	go test -run - -bench ReadAt4K .
func BenchmarkReadAt4K(b *testing.B) {
	for _, bc := range []struct {
		name     string
		maxBytes int64
	}{{"uncached", 0}, {"cached", defaultCacheBytes}} {
		b.Run(bc.name, func(b *testing.B) {
			ra := readerAt(b, openFixtureCache(b, bc.maxBytes), "/big.bin")
			buf := make([]byte, 4096)
			size := int64(len(fixtureBig()))
			b.SetBytes(4096)
			off := int64(0)
			for b.Loop() {
				if _, err := ra.ReadAt(buf, off); err != nil && err != io.EOF {
					b.Fatal(err)
				}
				if off += 4096; off+4096 > size {
					off = 0
				}
			}
		})
	}
}
