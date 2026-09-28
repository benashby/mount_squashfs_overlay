package main

import (
	"container/list"
	"errors"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"

	karp "github.com/KarpelesLab/squashfs"
)

// defaultCacheBytes bounds the decompressed-block cache (see -cache-mb).
const defaultCacheBytes = 256 << 20

// blockCache is an LRU of decompressed squashfs data blocks shared by every
// open file of one archive.
//
// KarpelesLab's Inode.ReadAt reads and decompresses every block a request
// touches, and keeps nothing between calls. WinFsp and FUSE split file reads
// into requests far smaller than a squashfs block (a 4 KiB read of a 1 MiB
// block decompresses 1 MiB), so without a cache sequential throughput drops
// roughly in proportion to blockSize/requestSize.
//
// Concurrent misses on the same block share one decompression.
type blockCache struct {
	maxBytes int64

	mu       sync.Mutex
	lru      *list.List // front = most recently used; values are *cacheEntry
	entries  map[blockKey]*list.Element
	inflight map[blockKey]*blockLoad
	bytes    int64

	hits, misses atomic.Int64
}

type blockKey struct {
	ino   uint32
	block int64
}

type cacheEntry struct {
	key  blockKey
	data []byte
}

type blockLoad struct {
	done chan struct{}
	data []byte
	err  error
}

// newBlockCache returns a cache holding up to maxBytes of block data, or nil
// (caching disabled) when maxBytes <= 0. A nil *blockCache is valid.
func newBlockCache(maxBytes int64) *blockCache {
	if maxBytes <= 0 {
		return nil
	}
	return &blockCache{
		maxBytes: maxBytes,
		lru:      list.New(),
		entries:  make(map[blockKey]*list.Element),
		inflight: make(map[blockKey]*blockLoad),
	}
}

// get returns the data of block key, calling load on a miss. The returned
// slice is shared and must not be modified.
func (c *blockCache) get(key blockKey, load func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		data := el.Value.(*cacheEntry).data
		c.mu.Unlock()
		c.hits.Add(1)
		return data, nil
	}
	if l, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-l.done
		c.hits.Add(1)
		return l.data, l.err
	}
	l := &blockLoad{done: make(chan struct{})}
	c.inflight[key] = l
	c.mu.Unlock()
	c.misses.Add(1)

	l.data, l.err = load()

	c.mu.Lock()
	delete(c.inflight, key)
	if l.err == nil && int64(len(l.data)) <= c.maxBytes {
		c.entries[key] = c.lru.PushFront(&cacheEntry{key: key, data: l.data})
		c.bytes += int64(len(l.data))
		for c.bytes > c.maxBytes {
			el := c.lru.Back()
			e := el.Value.(*cacheEntry)
			c.lru.Remove(el)
			delete(c.entries, e.key)
			c.bytes -= int64(len(e.data))
		}
	}
	c.mu.Unlock()
	close(l.done)
	return l.data, l.err
}

// cachedFile serves a regular squashfs file's reads block by block through
// the archive's blockCache.
type cachedFile struct {
	*io.SectionReader // Read/Seek, backed by cachedFile.ReadAt via cachedReaderAt
	file              fs.File
	ino               *karp.Inode
	blockSize         int64
	size              int64
	cache             *blockCache
}

var _ io.ReaderAt = (*cachedFile)(nil)

func newCachedFile(f fs.File, ino *karp.Inode, blockSize int64, cache *blockCache) *cachedFile {
	cf := &cachedFile{
		file:      f,
		ino:       ino,
		blockSize: blockSize,
		size:      int64(ino.Size),
		cache:     cache,
	}
	cf.SectionReader = io.NewSectionReader(cachedReaderAt{cf}, 0, cf.size)
	return cf
}

// cachedReaderAt breaks the cycle between the embedded SectionReader (whose
// ReadAt is promoted onto cachedFile) and the cached ReadAt it must call.
type cachedReaderAt struct{ f *cachedFile }

func (r cachedReaderAt) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }

func (f *cachedFile) Stat() (fs.FileInfo, error) { return f.file.Stat() }
func (f *cachedFile) Close() error               { return f.file.Close() }

// ReadAt implements io.ReaderAt with the same contract as the underlying
// Inode.ReadAt: short reads only at end of file, with io.EOF.
func (f *cachedFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("squashfs: negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < f.size {
		idx := off / f.blockSize
		data, err := f.block(idx)
		if err != nil {
			return n, err
		}
		within := off - idx*f.blockSize
		if within >= int64(len(data)) {
			return n, io.ErrUnexpectedEOF
		}
		c := copy(p[n:], data[within:])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// block returns the decompressed content of block idx (shorter than
// blockSize only for the file's last block).
func (f *cachedFile) block(idx int64) ([]byte, error) {
	return f.cache.get(blockKey{ino: f.ino.Ino, block: idx}, func() ([]byte, error) {
		start := idx * f.blockSize
		buf := make([]byte, min(f.blockSize, f.size-start))
		// Block-aligned, block-sized: Inode.ReadAt decompresses exactly this block.
		n, err := f.ino.ReadAt(buf, start)
		if n == len(buf) {
			return buf, nil
		}
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	})
}
