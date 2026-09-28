package main

// NOTE: always build with -tags "xz zstd" (see Makefile).
// Those tags activate comp_xz.go and comp_zstd.go inside
// github.com/KarpelesLab/squashfs; without them only gzip is supported.
//
// Two squashfs libraries are used for different purposes:
//   - KarpelesLab/squashfs  — FUSE reads (Open/Stat/ReadDir/Readlink)
//                             Inode.ReadAt decompresses one block per call;
//                             no full-file buffering, ~128 KB RAM per open file.
//   - CalebQ42/squashfs     — CoW copies only (copySquashToUpperAt)
//                             WriteTo decompresses blocks in parallel with a
//                             sync.Pool, using far less total allocation than
//                             KarpelesLab's per-call alloc pattern.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"

	caleb "github.com/CalebQ42/squashfs"
	karp "github.com/KarpelesLab/squashfs"
)

// dirIndex is a pre-built lowercase→realname map for one squashfs directory.
// Stored as the value type in SquashLayer.dirCache (a sync.Map).
type dirIndex map[string]string

// SquashLayer holds two readers over the same archive file:
//   - sb  (KarpelesLab) for per-block FUSE reads
//   - cq  (CalebQ42)    for parallel-WriteTo CoW copies
type SquashLayer struct {
	sb    *karp.Superblock
	cq    caleb.Reader
	size  int64
	cache *blockCache // nil when disabled
	// dirCache maps an fs-relative directory path → dirIndex (lowercase→realname).
	// Populated lazily on first case-insensitive miss for a given directory.
	// Safe to cache permanently: squashfs is immutable.
	// sync.Map gives lock-free loads after the cache is warm (the common case).
	dirCache sync.Map // map[string]dirIndex
}

// NewSquashLayer opens the archive and initialises both readers.
// Both libraries take io.ReaderAt so they share the same *os.File safely.
func NewSquashLayer(path string) (*SquashLayer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	sb, err := karp.New(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	cq, err := caleb.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	return &SquashLayer{sb: sb, cq: cq, size: info.Size(), cache: newBlockCache(defaultCacheBytes)}, nil
}

// SetCacheSize replaces the decompressed-block cache with one bounded to
// maxBytes; maxBytes <= 0 disables caching. Call before serving any reads.
func (s *SquashLayer) SetCacheSize(maxBytes int64) {
	s.cache = newBlockCache(maxBytes)
}

// Open returns an fs.File backed by a KarpelesLab Inode (implements io.ReaderAt).
// Regular files read through the archive's block cache when it is enabled.
func (s *SquashLayer) Open(fusePath string) (fs.File, error) {
	p := toFSPath(fusePath)
	f, err := s.sb.Open(p)
	if err != nil {
		if resolved := s.resolveCasePath(p); resolved != p {
			f, err = s.sb.Open(resolved)
		}
	}
	if err != nil || s.cache == nil {
		return f, err
	}
	if kf, ok := f.(*karp.File); ok {
		if ino, ok := kf.Sys().(*karp.Inode); ok {
			return newCachedFile(f, ino, int64(s.sb.BlockSize), s.cache), nil
		}
	}
	return f, nil
}

// Stat returns fs.FileInfo for the given FUSE path (follows symlinks).
func (s *SquashLayer) Stat(fusePath string) (fs.FileInfo, error) {
	p := toFSPath(fusePath)
	info, err := fs.Stat(s.sb, p)
	if err != nil {
		if resolved := s.resolveCasePath(p); resolved != p {
			info, err = fs.Stat(s.sb, resolved)
		}
	}
	return info, err
}

// ReadDir lists the directory at the given FUSE path.
func (s *SquashLayer) ReadDir(fusePath string) ([]fs.DirEntry, error) {
	p := toFSPath(fusePath)
	des, err := fs.ReadDir(s.sb, p)
	if err != nil {
		if resolved := s.resolveCasePath(p); resolved != p {
			des, err = fs.ReadDir(s.sb, resolved)
		}
	}
	return des, err
}

// ReadSymlink returns the symlink target at fusePath without following it.
func (s *SquashLayer) ReadSymlink(fusePath string) (string, error) {
	p := toFSPath(fusePath)
	inode, err := s.sb.FindInode(p, false)
	if err != nil {
		if resolved := s.resolveCasePath(p); resolved != p {
			inode, err = s.sb.FindInode(resolved, false)
		}
	}
	if err != nil {
		return "", err
	}
	target, err := inode.Readlink()
	if err != nil {
		return "", err
	}
	return string(target), nil
}

// CopyTo writes the full content of fusePath to dst using CalebQ42's parallel
// WriteTo path, which decompresses squashfs blocks concurrently (up to NumCPU
// goroutines) and recycles buffers via sync.Pool. This is used for CoW copies
// and is significantly cheaper in total allocations than KarpelesLab's ReadAt
// for sequential full-file reads.
func (s *SquashLayer) CopyTo(fusePath string, dst io.Writer) error {
	p := toFSPath(fusePath)
	f, err := s.cq.Open(p)
	if err != nil {
		if resolved := s.resolveCasePath(p); resolved != p {
			f, err = s.cq.Open(resolved)
			if err != nil {
				return fmt.Errorf("CopyTo %q (resolved %q): caleb Open: %w", p, resolved, err)
			}
		} else {
			return fmt.Errorf("CopyTo %q: caleb Open: %w", p, err)
		}
	}
	defer f.Close()
	if wt, ok := f.(io.WriterTo); ok {
		_, err = wt.WriteTo(dst)
	} else {
		_, err = io.Copy(dst, f)
	}
	return err
}

// Size returns the byte size of the raw .squashfs archive file.
func (s *SquashLayer) Size() int64 {
	return s.size
}

// ResolvePath returns fusePath with squashfs-canonical casing for every
// component. It is used by callers that need the correctly-cased path
// (e.g. when creating a whiteout so it matches the actual squashfs entry).
// Returns fusePath unchanged if it already has correct case or the path
// is not found in the archive.
func (s *SquashLayer) ResolvePath(fusePath string) string {
	p := toFSPath(fusePath)
	resolved := s.resolveCasePath(p)
	if resolved == p {
		return fusePath // already correct or not found — keep original
	}
	if resolved == "." {
		return "/"
	}
	return "/" + resolved
}

// resolveCasePath walks each component of fsPath case-insensitively against the
// squashfs directory listings, returning the correctly-cased path. If no match
// is found at any component the original fsPath is returned unchanged.
//
// Each directory's lowercase→realname map is built once on the first miss and
// stored in dirCache forever (squashfs is immutable, so the cache never stales).
func (s *SquashLayer) resolveCasePath(fsPath string) string {
	if fsPath == "" || fsPath == "." {
		return fsPath
	}
	parts := strings.Split(fsPath, "/")
	current := "."
	for _, part := range parts {
		if part == "" {
			continue
		}
		lpart := strings.ToLower(part)

		// Fast path: check the per-directory cache first (lock-free load).
		var idx dirIndex
		if v, ok := s.dirCache.Load(current); ok {
			idx = v.(dirIndex)
		} else {
			// Build the lowercase index for this directory.
			entries, err := fs.ReadDir(s.sb, current)
			if err != nil {
				return fsPath
			}
			newIdx := make(dirIndex, len(entries))
			for _, e := range entries {
				newIdx[strings.ToLower(e.Name())] = e.Name()
			}
			// LoadOrStore: if another goroutine raced us, use their copy.
			if actual, loaded := s.dirCache.LoadOrStore(current, newIdx); loaded {
				idx = actual.(dirIndex)
			} else {
				idx = newIdx
			}
		}

		matched, ok := idx[lpart]
		if !ok {
			return fsPath
		}
		if current == "." {
			current = matched
		} else {
			current = current + "/" + matched
		}
	}
	return current
}

// toFSPath converts a FUSE absolute path ("/dir/file" or "\dir\file") to an fs.FS-style
// relative path ("dir/file"), mapping "/" or "\" to ".".
func toFSPath(fusePath string) string {
	// Normalize Windows backslashes to Unix forward slashes
	p := strings.ReplaceAll(fusePath, "\\", "/")
	p = strings.TrimPrefix(p, "/")
	// Strip any remaining leading slashes (e.g. from "//dir/file").
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	if p == "" {
		return "."
	}
	return p
}
