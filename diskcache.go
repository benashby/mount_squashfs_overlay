package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The disk cache keeps a local, sparse copy of each squashfs image it reads,
// filled one chunk at a time as the image is read. A chunk read once from the
// (usually remote) image is served from local disk afterwards, including after
// a remount or reboot. It stores the image's raw bytes, so it works below both
// squashfs libraries and caches metadata tables along with file data.
//
// Space is managed without user involvement: when the cache is larger than
// its size cap, or the disk holding it has less free space than the floor,
// the least recently used chunks across every image in the cache directory are
// released (hole-punched) until both limits hold again.
//
// Per image, the cache directory holds:
//
//	<id>.data  sparse file, same size and layout as the image
//	<id>.idx   header + one 8-byte entry per chunk (crc32c, last access)
//	<id>.lock  held open and locked while a process uses the image
const (
	idxMagic        = "SQOVDC01"
	idxHeaderSize   = 4096
	idxEntrySize    = 8
	atimeFlushEvery = 30 * time.Second
)

// Variables so tests can use small values.
var (
	diskChunkSize   int64 = 4 << 20
	evictSlackBytes int64 = 1 << 30 // free this much extra per eviction run
)

var (
	errCacheBusy   = errors.New("disk cache: image is in use by another process")
	errChunkAbsent = errors.New("disk cache: chunk not cached and image unreachable")
	errEvicted     = errors.New("disk cache: chunk evicted")
	crcTable       = crc32.MakeTable(crc32.Castagnoli)
	atimeEpoch     = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
)

// DiskCache manages one cache directory for all images opened by this process.
type DiskCache struct {
	dir          string
	maxBytes     int64
	minFreeBytes int64

	// Replaceable in tests.
	freeSpace func(dir string) (int64, error)
	now       func() time.Time

	mu     sync.Mutex // guards images and closedUsage, serializes eviction
	images map[string]*CachedImage

	// Bytes held by caches this process does not have open, by index path,
	// remembered with the index file's mtime and size.
	closedUsage map[string]closedUsage

	foreground atomic.Int32 // fetches on behalf of readers; prefetch yields to them
}

// NewDiskCache opens (creating if needed) a cache directory.
// maxBytes caps the space used by cached chunks; minFreeBytes is the free
// space to leave on the disk. Zero disables either limit.
func NewDiskCache(dir string, maxBytes, minFreeBytes int64) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DiskCache{
		dir:          dir,
		maxBytes:     maxBytes,
		minFreeBytes: minFreeBytes,
		freeSpace:    diskFreeSpace,
		now:          time.Now,
		images:       make(map[string]*CachedImage),
		closedUsage:  make(map[string]closedUsage),
	}, nil
}

// idxHeader identifies the image a cache belongs to.
type idxHeader struct {
	chunkSize uint32
	size      int64
	mtime     int64 // unix nanoseconds
	sbHash    [32]byte
	path      string // absolute path the image was opened from (for offline lookup)
}

func (h *idxHeader) marshal() []byte {
	b := make([]byte, idxHeaderSize)
	copy(b, idxMagic)
	binary.LittleEndian.PutUint32(b[8:], 1) // version
	binary.LittleEndian.PutUint32(b[12:], h.chunkSize)
	binary.LittleEndian.PutUint64(b[16:], uint64(h.size))
	binary.LittleEndian.PutUint64(b[24:], uint64(h.mtime))
	copy(b[32:64], h.sbHash[:])
	p := h.path
	if len(p) > idxHeaderSize-66 {
		p = p[:idxHeaderSize-66]
	}
	binary.LittleEndian.PutUint16(b[64:], uint16(len(p)))
	copy(b[66:], p)
	return b
}

func unmarshalIdxHeader(b []byte) (*idxHeader, error) {
	if len(b) < idxHeaderSize || string(b[:8]) != idxMagic || binary.LittleEndian.Uint32(b[8:]) != 1 {
		return nil, errors.New("disk cache: bad index header")
	}
	h := &idxHeader{
		chunkSize: binary.LittleEndian.Uint32(b[12:]),
		size:      int64(binary.LittleEndian.Uint64(b[16:])),
		mtime:     int64(binary.LittleEndian.Uint64(b[24:])),
	}
	copy(h.sbHash[:], b[32:64])
	n := int(binary.LittleEndian.Uint16(b[64:]))
	if 66+n > len(b) {
		return nil, errors.New("disk cache: bad index header")
	}
	h.path = string(b[66 : 66+n])
	return h, nil
}

func (h *idxHeader) id() string {
	d := sha256.New()
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:], uint64(h.size))
	binary.LittleEndian.PutUint64(b[8:], uint64(h.mtime))
	d.Write(b[:])
	d.Write(h.sbHash[:])
	return hex.EncodeToString(d.Sum(nil)[:12])
}

func samePath(a, b string) bool {
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// chunkEntry is one index entry. atime == 0 means the chunk is not cached.
type chunkEntry struct {
	crc   uint32
	atime uint32 // minutes since atimeEpoch, at least 1 when cached
}

// CachedImage is an io.ReaderAt over one image, served through the disk cache.
type CachedImage struct {
	dc      *DiskCache
	id      string
	hdr     *idxHeader
	src     io.ReaderAt // nil when the image was unreachable at open
	srcFile *os.File
	data    *os.File
	idx     *os.File
	lock    *os.File

	mu           sync.Mutex
	entries      []chunkEntry
	verified     []bool  // chunk checked against its crc by this process
	pins         []int32 // readers currently using the chunk; eviction skips pinned chunks
	inflight     map[int]*chunkLoad
	dirty        bool // atimes changed since the last flush
	presentBytes int64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type chunkLoad struct {
	done chan struct{}
	err  error
}

// Open returns a cached reader for the image at path. If the image cannot be
// opened but a cache for the same path exists, it is served from the cache
// alone (reads of chunks that were never cached fail). errCacheBusy means
// another process holds this image's cache; callers should read the image
// directly instead.
func (dc *DiskCache) Open(path string) (*CachedImage, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	hdr, srcFile, err := readImageHeader(abs)
	if err != nil {
		offline, oerr := dc.findByPath(abs)
		if oerr != nil || offline == nil {
			return nil, err
		}
		hdr = offline
	}
	img, err := dc.openImage(hdr, srcFile)
	if err != nil {
		if srcFile != nil {
			srcFile.Close()
		}
		return nil, err
	}
	if srcFile != nil {
		dc.removeStale(abs, img.id)
	}
	return img, nil
}

func readImageHeader(abs string) (*idxHeader, *os.File, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	sb := make([]byte, 96) // squashfs superblock
	if _, err := f.ReadAt(sb, 0); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("reading superblock: %w", err)
	}
	return &idxHeader{
		chunkSize: uint32(diskChunkSize),
		size:      st.Size(),
		mtime:     st.ModTime().UnixNano(),
		sbHash:    sha256.Sum256(sb),
		path:      abs,
	}, f, nil
}

func (dc *DiskCache) openImage(hdr *idxHeader, srcFile *os.File) (*CachedImage, error) {
	id := hdr.id()
	base := filepath.Join(dc.dir, id)

	lock, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, errCacheBusy
	}
	img := &CachedImage{dc: dc, id: id, hdr: hdr, lock: lock, inflight: make(map[int]*chunkLoad), stop: make(chan struct{})}
	if srcFile != nil {
		img.src, img.srcFile = srcFile, srcFile
	}
	fail := func(err error) (*CachedImage, error) {
		img.closeFiles()
		return nil, err
	}

	n := int((hdr.size + diskChunkSize - 1) / diskChunkSize)
	img.entries = make([]chunkEntry, n)
	img.verified = make([]bool, n)
	img.pins = make([]int32, n)

	if img.idx, err = os.OpenFile(base+".idx", os.O_CREATE|os.O_RDWR, 0o644); err != nil {
		return fail(err)
	}
	if img.data, err = os.OpenFile(base+".data", os.O_CREATE|os.O_RDWR, 0o644); err != nil {
		return fail(err)
	}
	if !img.loadIndex() {
		// New or unusable index: start empty.
		if err := img.idx.Truncate(0); err != nil {
			return fail(err)
		}
		if err := img.data.Truncate(0); err != nil {
			return fail(err)
		}
		if err := setSparse(img.data); err != nil {
			return fail(err)
		}
		if err := img.data.Truncate(hdr.size); err != nil {
			return fail(err)
		}
		if _, err := img.idx.WriteAt(hdr.marshal(), 0); err != nil {
			return fail(err)
		}
		if err := img.idx.Truncate(idxHeaderSize + int64(n)*idxEntrySize); err != nil {
			return fail(err)
		}
	}

	dc.mu.Lock()
	dc.images[id] = img
	dc.mu.Unlock()

	img.wg.Add(1)
	go img.flushLoop()
	return img, nil
}

// loadIndex reads an existing index into img. It returns false if there is no
// usable index for this image.
func (img *CachedImage) loadIndex() bool {
	st, err := img.idx.Stat()
	if err != nil || st.Size() != idxHeaderSize+int64(len(img.entries))*idxEntrySize {
		return false
	}
	buf := make([]byte, st.Size())
	if _, err := img.idx.ReadAt(buf, 0); err != nil {
		return false
	}
	hdr, err := unmarshalIdxHeader(buf)
	if err != nil || hdr.id() != img.id || int64(hdr.chunkSize) != diskChunkSize {
		return false
	}
	if img.hdr.path == "" {
		img.hdr.path = hdr.path
	}
	for i := range img.entries {
		e := buf[idxHeaderSize+i*idxEntrySize:]
		img.entries[i] = chunkEntry{crc: binary.LittleEndian.Uint32(e), atime: binary.LittleEndian.Uint32(e[4:])}
		if img.entries[i].atime != 0 {
			img.presentBytes += img.chunkLen(i)
		}
	}
	return true
}

// findByPath returns the header of a cache made from the image at abs.
func (dc *DiskCache) findByPath(abs string) (*idxHeader, error) {
	var best *idxHeader
	var bestTime time.Time
	for _, p := range dc.indexFiles() {
		hdr, err := readIdxHeaderFile(p)
		if err != nil || !samePath(hdr.path, abs) {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.ModTime().After(bestTime) {
			best, bestTime = hdr, st.ModTime()
		}
	}
	return best, nil
}

// removeStale deletes caches of earlier versions of the image at abs.
func (dc *DiskCache) removeStale(abs, keepID string) {
	for _, p := range dc.indexFiles() {
		id := strings.TrimSuffix(filepath.Base(p), ".idx")
		if id == keepID {
			continue
		}
		if hdr, err := readIdxHeaderFile(p); err == nil && samePath(hdr.path, abs) {
			dc.deleteCache(id)
		}
	}
}

// deleteCache removes an image cache unless another process holds it.
func (dc *DiskCache) deleteCache(id string) bool {
	base := filepath.Join(dc.dir, id)
	lock, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	if lockFile(lock) != nil {
		lock.Close()
		return false
	}
	os.Remove(base + ".idx")
	os.Remove(base + ".data")
	unlockFile(lock)
	lock.Close()
	os.Remove(base + ".lock")
	return true
}

func (dc *DiskCache) indexFiles() []string {
	m, _ := filepath.Glob(filepath.Join(dc.dir, "*.idx"))
	return m
}

func readIdxHeaderFile(p string) (*idxHeader, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, idxHeaderSize)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, err
	}
	return unmarshalIdxHeader(b)
}

func (img *CachedImage) chunkLen(i int) int64 {
	return min(diskChunkSize, img.hdr.size-int64(i)*diskChunkSize)
}

// Size returns the image size.
func (img *CachedImage) Size() int64 { return img.hdr.size }

// Online reports whether the image itself is reachable.
func (img *CachedImage) Online() bool { return img.src != nil }

// ReadAt implements io.ReaderAt.
func (img *CachedImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("disk cache: negative offset")
	}
	if off >= img.hdr.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < img.hdr.size {
		i := int(off / diskChunkSize)
		end := min(int64(i+1)*diskChunkSize, img.hdr.size, off+int64(len(p)-n))
		img.pin(i)
		err := img.ensure(i, true)
		if err == nil {
			var c int
			c, err = img.data.ReadAt(p[n:n+int(end-off)], off)
			n += c
			off += int64(c)
		}
		img.unpin(i)
		if err != nil {
			return n, err
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (img *CachedImage) pin(i int) {
	img.mu.Lock()
	img.pins[i]++
	img.mu.Unlock()
}

func (img *CachedImage) unpin(i int) {
	img.mu.Lock()
	img.pins[i]--
	img.mu.Unlock()
}

func (img *CachedImage) nowMinutes() uint32 {
	return uint32(max(1, img.dc.now().Sub(atimeEpoch)/time.Minute))
}

// ensure makes chunk i present locally and verified by this process.
func (img *CachedImage) ensure(i int, foreground bool) error {
	img.mu.Lock()
	for {
		e := &img.entries[i]
		if e.atime != 0 && img.verified[i] {
			if t := img.nowMinutes(); e.atime != t {
				e.atime, img.dirty = t, true
			}
			img.mu.Unlock()
			return nil
		}
		l, ok := img.inflight[i]
		if !ok {
			break
		}
		// Another reader is loading the chunk, or it is being evicted.
		// Wait, then look again.
		img.mu.Unlock()
		<-l.done
		if l.err != nil && l.err != errEvicted {
			return l.err
		}
		img.mu.Lock()
	}
	l := &chunkLoad{done: make(chan struct{})}
	img.inflight[i] = l
	present, want := img.entries[i].atime != 0, img.entries[i].crc
	img.mu.Unlock()

	added := int64(0)
	buf := make([]byte, img.chunkLen(i))
	start := int64(i) * diskChunkSize
	ok := false
	if present {
		// First use of a cached chunk in this process: check it survived intact.
		if _, err := img.data.ReadAt(buf, start); err == nil && crc32.Checksum(buf, crcTable) == want {
			ok = true
		}
	}
	if !ok {
		l.err = img.fetch(i, buf, foreground)
		if l.err == nil && !present {
			added = int64(len(buf))
		}
	}

	img.mu.Lock()
	delete(img.inflight, i)
	if l.err == nil {
		img.verified[i] = true
		if ok {
			img.entries[i].atime, img.dirty = img.nowMinutes(), true
		}
		img.presentBytes += added
	}
	img.mu.Unlock()
	close(l.done)

	if added > 0 {
		img.dc.maybeEvict()
	}
	return l.err
}

// fetch reads chunk i from the image into buf, stores it and records it in
// the index. The data is written before its index entry, so a crash can only
// leave an entry whose crc does not match, which ensure then refetches.
func (img *CachedImage) fetch(i int, buf []byte, foreground bool) error {
	if img.src == nil {
		return errChunkAbsent
	}
	if foreground {
		img.dc.foreground.Add(1)
		defer img.dc.foreground.Add(-1)
	}
	start := int64(i) * diskChunkSize
	if n, err := img.src.ReadAt(buf, start); n != len(buf) {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if _, err := img.data.WriteAt(buf, start); err != nil {
		return err
	}
	ent := chunkEntry{crc: crc32.Checksum(buf, crcTable), atime: img.nowMinutes()}
	img.mu.Lock()
	img.entries[i] = ent
	img.mu.Unlock()
	return img.writeEntry(i, ent)
}

func (img *CachedImage) writeEntry(i int, e chunkEntry) error {
	var b [idxEntrySize]byte
	binary.LittleEndian.PutUint32(b[:], e.crc)
	binary.LittleEndian.PutUint32(b[4:], e.atime)
	_, err := img.idx.WriteAt(b[:], idxHeaderSize+int64(i)*idxEntrySize)
	return err
}

// flush writes every entry's last-access time to the index.
func (img *CachedImage) flush() error {
	img.mu.Lock()
	if !img.dirty {
		img.mu.Unlock()
		return nil
	}
	buf := make([]byte, len(img.entries)*idxEntrySize)
	for i, e := range img.entries {
		binary.LittleEndian.PutUint32(buf[i*idxEntrySize:], e.crc)
		binary.LittleEndian.PutUint32(buf[i*idxEntrySize+4:], e.atime)
	}
	img.dirty = false
	img.mu.Unlock()
	_, err := img.idx.WriteAt(buf, idxHeaderSize)
	return err
}

func (img *CachedImage) flushLoop() {
	defer img.wg.Done()
	t := time.NewTicker(atimeFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-img.stop:
			return
		case <-t.C:
			img.flush()
		}
	}
}

// Prefetch copies every chunk that is not cached yet, yielding to readers,
// until the image is fully cached, the cache limits would force an eviction,
// or the image is closed. It returns the number of chunks fetched.
func (img *CachedImage) Prefetch() (int, error) {
	fetched := 0
	for i := range img.entries {
		select {
		case <-img.stop:
			return fetched, nil
		default:
		}
		img.mu.Lock()
		present := img.entries[i].atime != 0
		img.mu.Unlock()
		if present {
			continue
		}
		if !img.dc.hasRoomFor(img.chunkLen(i)) {
			return fetched, nil
		}
		for img.dc.foreground.Load() > 0 {
			select {
			case <-img.stop:
				return fetched, nil
			case <-time.After(20 * time.Millisecond):
			}
		}
		if err := img.ensure(i, false); err != nil {
			return fetched, err
		}
		fetched++
	}
	return fetched, nil
}

// StartPrefetch runs Prefetch in the background; Close waits for it.
// done, if not nil, is called with Prefetch's result.
func (img *CachedImage) StartPrefetch(done func(fetched int, err error)) {
	img.wg.Add(1)
	go func() {
		defer img.wg.Done()
		n, err := img.Prefetch()
		if done != nil {
			done(n, err)
		}
	}()
}

// Close flushes the index and releases the image.
func (img *CachedImage) Close() error {
	img.stopOnce.Do(func() { close(img.stop) })
	img.wg.Wait()
	err := img.flush()
	img.dc.mu.Lock()
	delete(img.dc.images, img.id)
	img.dc.mu.Unlock()
	img.closeFiles()
	return err
}

func (img *CachedImage) closeFiles() {
	for _, f := range []*os.File{img.data, img.idx, img.srcFile} {
		if f != nil {
			f.Close()
		}
	}
	if img.lock != nil {
		unlockFile(img.lock)
		img.lock.Close()
	}
}

// ── Space management ──────────────────────────────────────────────────

type closedUsage struct {
	mtime time.Time
	size  int64
	bytes int64
}

// usage returns the bytes held by every cache in the directory. Callers hold dc.mu.
func (dc *DiskCache) usage() int64 {
	var total int64
	for _, img := range dc.images {
		img.mu.Lock()
		total += img.presentBytes
		img.mu.Unlock()
	}
	seen := map[string]bool{}
	for _, p := range dc.indexFiles() {
		if dc.images[strings.TrimSuffix(filepath.Base(p), ".idx")] != nil {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		seen[p] = true
		u, ok := dc.closedUsage[p]
		if !ok || !u.mtime.Equal(st.ModTime()) || u.size != st.Size() {
			u = closedUsage{st.ModTime(), st.Size(), presentBytesInIndexFile(p)}
			dc.closedUsage[p] = u
		}
		total += u.bytes
	}
	for p := range dc.closedUsage {
		if !seen[p] {
			delete(dc.closedUsage, p)
		}
	}
	return total
}

func presentBytesInIndexFile(p string) int64 {
	b, err := os.ReadFile(p)
	if err != nil || len(b) < idxHeaderSize {
		return 0
	}
	hdr, err := unmarshalIdxHeader(b)
	if err != nil {
		return 0
	}
	var total int64
	for i := 0; idxHeaderSize+(i+1)*idxEntrySize <= len(b); i++ {
		if binary.LittleEndian.Uint32(b[idxHeaderSize+i*idxEntrySize+4:]) != 0 {
			total += min(diskChunkSize, hdr.size-int64(i)*diskChunkSize)
		}
	}
	return total
}

// overBy returns how many bytes must be released to satisfy both limits
// after adding extra bytes.
func (dc *DiskCache) overBy(extra int64) int64 {
	need := int64(0)
	if dc.maxBytes > 0 {
		need = max(need, dc.usage()+extra-dc.maxBytes)
	}
	if dc.minFreeBytes > 0 {
		if free, err := dc.freeSpace(dc.dir); err == nil {
			need = max(need, dc.minFreeBytes-(free-extra))
		}
	}
	return need
}

func (dc *DiskCache) hasRoomFor(n int64) bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return dc.overBy(n) <= 0
}

// evictCandidate is one cached chunk that may be released.
type evictCandidate struct {
	atime uint32
	img   *CachedImage
	i     int
	bytes int64
}

// maybeEvict releases least recently used chunks until the limits hold,
// plus some slack so eviction does not run on every fetch.
func (dc *DiskCache) maybeEvict() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	need := dc.overBy(0)
	if need <= 0 {
		return
	}
	need += evictSlackBytes

	// Caches no process has open are loaded (and locked) so their chunks can
	// compete on age with the open ones.
	var borrowed []*CachedImage
	defer func() {
		for _, img := range borrowed {
			if img.idx != nil {
				img.flushNow()
				img.closeFiles()
			}
		}
	}()
	var cands []evictCandidate
	for _, p := range dc.indexFiles() {
		id := strings.TrimSuffix(filepath.Base(p), ".idx")
		img := dc.images[id]
		if img == nil {
			if img = dc.borrow(p); img == nil {
				continue // held by another process
			}
			borrowed = append(borrowed, img)
		}
		img.mu.Lock()
		for i, e := range img.entries {
			if e.atime != 0 && img.pins[i] == 0 && img.inflight[i] == nil {
				cands = append(cands, evictCandidate{e.atime, img, i, img.chunkLen(i)})
			}
		}
		img.mu.Unlock()
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].atime < cands[b].atime })

	for _, c := range cands {
		if need <= 0 {
			break
		}
		if c.img.evict(c.i) {
			need -= c.bytes
		}
	}
	for _, img := range borrowed {
		if img.presentBytes == 0 {
			img.flushNow()
			img.closeFiles()
			base := filepath.Join(dc.dir, img.id)
			os.Remove(base + ".idx")
			os.Remove(base + ".data")
			os.Remove(base + ".lock")
			img.data, img.idx, img.lock = nil, nil, nil
		}
		delete(dc.closedUsage, filepath.Join(dc.dir, img.id)+".idx")
	}
}

// borrow opens and locks a cache that no process is using, for eviction.
func (dc *DiskCache) borrow(idxPath string) *CachedImage {
	hdr, err := readIdxHeaderFile(idxPath)
	if err != nil {
		return nil
	}
	base := strings.TrimSuffix(idxPath, ".idx")
	lock, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil
	}
	if lockFile(lock) != nil {
		lock.Close()
		return nil
	}
	n := int((hdr.size + diskChunkSize - 1) / diskChunkSize)
	img := &CachedImage{dc: dc, id: hdr.id(), hdr: hdr, lock: lock,
		entries: make([]chunkEntry, n), verified: make([]bool, n), pins: make([]int32, n),
		inflight: map[int]*chunkLoad{}}
	if img.idx, err = os.OpenFile(idxPath, os.O_RDWR, 0); err == nil {
		img.data, err = os.OpenFile(base+".data", os.O_RDWR, 0)
	}
	if err != nil || !img.loadIndex() {
		img.closeFiles()
		return nil
	}
	return img
}

// evict releases chunk i. The index entry is cleared before the space is
// released, so a crash in between only leaks space until the chunk is
// fetched again.
func (img *CachedImage) evict(i int) bool {
	img.mu.Lock()
	e := img.entries[i]
	if e.atime == 0 || img.pins[i] != 0 || img.inflight[i] != nil {
		img.mu.Unlock()
		return false
	}
	// Readers arriving now wait on this until the space is released, so a
	// refetch cannot land before the hole punch and be zeroed by it.
	l := &chunkLoad{done: make(chan struct{}), err: errEvicted}
	img.inflight[i] = l
	img.entries[i] = chunkEntry{}
	img.verified[i] = false
	img.presentBytes -= img.chunkLen(i)
	img.mu.Unlock()

	img.writeEntry(i, chunkEntry{})
	punchHole(img.data, int64(i)*diskChunkSize, img.chunkLen(i))

	img.mu.Lock()
	delete(img.inflight, i)
	img.mu.Unlock()
	close(l.done)
	return true
}

func (img *CachedImage) flushNow() {
	img.mu.Lock()
	img.dirty = true
	img.mu.Unlock()
	img.flush()
}
