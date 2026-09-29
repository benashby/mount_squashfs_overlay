//go:build !windows

package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// Whiteout conventions (compatible with Docker/OCI overlayfs):
//
//	.wh.<name>        — marks <name> as deleted in the lower (squashfs) layer
//	.wh..wh..opq      — marks the directory as opaque (hides all lower-layer contents)
const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
	invalidFH      = ^uint64(0) // cgofuse sentinel for "no file handle"
)

// handle represents an open file descriptor inside the overlay filesystem.
//
// upperFile and sqFile are mutually exclusive and set exactly once at Open
// time before the handle is published via storeHandle.  After that point
// neither field is ever reassigned, so there is no mutation race: any
// goroutine that loads the handle via loadHandle sees the fully-initialised
// struct thanks to sync.Map's happens-before guarantee.
//
// upperFile — non-nil when the file lives in (or was CoW'd into) the upper
//             directory.  Written eagerly in Open for any write-mode open, or
//             already present if the file was in upper.
// sqFile    — non-nil for read-only squashfs files.  The VFS rejects write(2)
//             on O_RDONLY fds before the call reaches FUSE, so sqFile handles
//             never receive a Write callback.
type handle struct {
	upperFile atomic.Pointer[os.File] // set once at open; nil means squashfs-backed
	sqFile    fs.File                 // non-nil for read-only squashfs files; implements io.ReaderAt
}

// OverlayFS is a cgofuse FileSystemInterface that presents a union of:
//   - lower layer: a read-only squashfs archive
//   - upper layer: a writable directory on disk (persistent)
//
// Reads consult the upper directory first; writes always go to upper.
// Deletions are represented by whiteout marker files so they survive remount.
type OverlayFS struct {
	fuse.FileSystemBase

	squash   *SquashLayer
	upperDir string // absolute OS path to the upper directory
	debug    bool

	fhs    sync.Map // map[uint64]*handle
	nextFH atomic.Uint64
}

// --- Lifecycle -----------------------------------------------------------

func (o *OverlayFS) Init() {}
func (o *OverlayFS) Destroy() {
	o.fhs.Range(func(_, v any) bool {
		h := v.(*handle)
		if uf := h.upperFile.Load(); uf != nil {
			uf.Close()
		}
		if h.sqFile != nil {
			h.sqFile.Close()
		}
		return true
	})
}

// --- Metadata ------------------------------------------------------------

func (o *OverlayFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	// Fast path: use already-open upper file handle.
	if fh != invalidFH {
		if h, ok := o.loadHandle(fh); ok {
			if uf := h.upperFile.Load(); uf != nil {
				if info, err := uf.Stat(); err == nil {
					fillStat(stat, info)
					return 0
				}
			}
		}
	}
	return o.getattrByPath(path, stat)
}

func (o *OverlayFS) getattrByPath(path string, stat *fuse.Stat_t) int {
	// 1. Upper layer wins.
	if info, err := os.Lstat(o.upper(path)); err == nil {
		fillStat(stat, info)
		return 0
	}
	// 2. Check whiteout — path was deleted.
	if o.hasWhiteout(path) {
		return -fuse.ENOENT
	}
	// 3. Fall back to squashfs.
	info, err := o.squash.Stat(path)
	if err != nil {
		return -fuse.ENOENT
	}
	fillStat(stat, info)
	return 0
}

func (o *OverlayFS) Access(path string, mask uint32) int {
	return 0 // no ACL enforcement; all access permitted
}

func (o *OverlayFS) Chmod(path string, mode uint32) int {
	up := o.upper(path)
	if _, err := os.Lstat(up); err == nil {
		return errToFuse(os.Chmod(up, fs.FileMode(mode)))
	}
	return 0 // squashfs is read-only; silently accept
}

func (o *OverlayFS) Chown(path string, uid uint32, gid uint32) int {
	return 0 // ownership is not meaningful on Windows; accept silently
}

func (o *OverlayFS) Utimens(path string, tmsp []fuse.Timespec) int {
	up := o.upper(path)
	if _, err := os.Lstat(up); err == nil && len(tmsp) >= 2 {
		atime := time.Unix(tmsp[0].Sec, tmsp[0].Nsec)
		mtime := time.Unix(tmsp[1].Sec, tmsp[1].Nsec)
		return errToFuse(os.Chtimes(up, atime, mtime))
	}
	return 0
}

func (o *OverlayFS) Statfs(path string, stat *fuse.Statfs_t) int {
	const blockSize = 4096
	sqBlocks := uint64(o.squash.Size()) / blockSize
	stat.Bsize = blockSize
	stat.Frsize = blockSize
	stat.Blocks = sqBlocks
	stat.Bfree = 1 << 20  // report 4 GiB free (writes go to the host FS)
	stat.Bavail = 1 << 20
	stat.Files = 1 << 16
	stat.Ffree = 1 << 16
	stat.Namemax = 255
	return 0
}

// --- Directory operations ------------------------------------------------

func (o *OverlayFS) Mkdir(path string, mode uint32) int {
	if o.upperDir == "" {
		return -fuse.EROFS
	}
	up := o.upper(path)
	if err := os.MkdirAll(up, fs.FileMode(mode)|0111); err != nil {
		return -fuse.EIO
	}
	o.removeWhiteout(path)
	return 0
}

func (o *OverlayFS) Rmdir(path string) int {
	if o.upperDir == "" {
		return -fuse.EROFS
	}
	up := o.upper(path)
	// Remove upper copy if present.
	_ = os.Remove(up)
	// If the directory exists in squashfs, plant a whiteout using the
	// squashfs-canonical path so hasWhiteout() finds it on later access.
	canonical := o.squash.ResolvePath(path)
	if _, err := o.squash.Stat(canonical); err == nil {
		o.createWhiteout(canonical)
	}
	return 0
}

func (o *OverlayFS) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64, fh uint64,
) int {
	if o.debug {
		fmt.Fprintf(os.Stderr, "READDIR path=%q ofst=%d\n", path, ofst)
	}

	// Collect real children, tracking which layer each name came from.
	// inUpper[name] = true  → entry lives in the upper directory
	// inUpper[name] = false → entry is squashfs-only
	names := make([]string, 0)
	inUpper := make(map[string]bool)
	whiteouts := make(map[string]bool)
	opaque := false

	// Upper layer: collect real entries and whiteout markers.
	upPath := o.upper(path)
	if des, err := os.ReadDir(upPath); err == nil {
		for _, de := range des {
			name := de.Name()
			switch {
			case name == opaqueWhiteout:
				opaque = true
			case strings.HasPrefix(name, whiteoutPrefix):
				// Lowercase the key so that a whiteout for "config.ini"
				// suppresses a squashfs entry named "Config.ini" and vice-versa.
				whiteouts[strings.ToLower(name[len(whiteoutPrefix):])] = true
			default:
				names = append(names, name)
				inUpper[name] = true
			}
		}
	}

	// Lower layer: add squashfs entries not hidden by whiteouts.
	if !opaque {
		if sqDes, err := o.squash.ReadDir(path); err == nil {
			for _, de := range sqDes {
				name := de.Name()
				if whiteouts[strings.ToLower(name)] {
					continue
				}
				if !inUpper[name] {
					names = append(names, name)
					inUpper[name] = false
				}
			}
		}
	}

	// Sort children for consistent ordering.
	sort.Strings(names)

	if o.debug {
		fmt.Fprintf(os.Stderr, "READDIR: %d children collected\n", len(names))
	}

	// Fill "." and ".." first.
	var dotSt fuse.Stat_t
	o.getattrByPath(path, &dotSt)
	fill(".", &dotSt, 0)
	fill("..", nil, 0)

	// Fill all child entries. We already know which layer each name belongs to,
	// so go straight to that layer — no whiteout re-check needed.
	// For upper entries reuse upPath (already computed above) to avoid
	// re-calling upper() once per entry.
	dirPrefix := strings.TrimRight(path, "/")
	for _, name := range names {
		var st fuse.Stat_t
		var info fs.FileInfo
		var err error
		if inUpper[name] {
			info, err = os.Lstat(upPath + "/" + name)
		} else {
			info, err = o.squash.Stat(dirPrefix + "/" + name)
		}
		if err == nil {
			fillStat(&st, info)
			if o.debug {
				fmt.Fprintf(os.Stderr, "  fill %q mode=0%o size=%d\n", name, st.Mode, st.Size)
			}
			fill(name, &st, 0)
		} else {
			if o.debug {
				fmt.Fprintf(os.Stderr, "  SKIP %q (stat failed: %v)\n", name, err)
			}
		}
	}

	if o.debug {
		fmt.Fprintf(os.Stderr, "READDIR done\n")
	}
	return 0
}

// --- File creation / deletion --------------------------------------------

func (o *OverlayFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if o.upperDir == "" {
		return -fuse.EROFS, invalidFH
	}
	up := o.upper(path)
	if err := os.MkdirAll(filepath.Dir(up), 0755); err != nil {
		return -fuse.EIO, invalidFH
	}
	o.removeWhiteout(path)

	f, err := os.OpenFile(up, flags|os.O_CREATE, fs.FileMode(mode))
	if err != nil {
		return -fuse.EIO, invalidFH
	}
	h := &handle{}
	h.upperFile.Store(f)
	return 0, o.storeHandle(h)
}

func (o *OverlayFS) Unlink(path string) int {
	if o.upperDir == "" {
		return -fuse.EROFS
	}
	up := o.upper(path)
	_ = os.Remove(up)
	// Use the squashfs-canonical path for the whiteout so that subsequent
	// Getattr/Open calls with the correct case find it via hasWhiteout().
	canonical := o.squash.ResolvePath(path)
	if _, err := o.squash.Stat(canonical); err == nil {
		o.createWhiteout(canonical)
	}
	return 0
}

func (o *OverlayFS) Rename(oldpath string, newpath string) int {
	if o.upperDir == "" {
		return -fuse.EROFS
	}
	oldUp := o.upper(oldpath)
	newUp := o.upper(newpath)

	if err := os.MkdirAll(filepath.Dir(newUp), 0755); err != nil {
		return -fuse.EIO
	}

	existsInUpper := false
	if _, err := os.Lstat(oldUp); err == nil {
		existsInUpper = true
	}

	if existsInUpper {
		if err := os.Rename(oldUp, newUp); err != nil {
			return -fuse.EIO
		}
	} else {
		// Source only in squashfs: CoW it to the new upper location.
		if _, err := o.squash.Stat(oldpath); err != nil {
			return -fuse.ENOENT
		}
		if err := o.copySquashToUpperAt(oldpath, newpath); err != nil {
			return -fuse.EIO
		}
	}

	// If oldpath existed in squashfs, hide it with a whiteout using the
	// squashfs-canonical casing.
	oldCanonical := o.squash.ResolvePath(oldpath)
	if _, err := o.squash.Stat(oldCanonical); err == nil {
		o.createWhiteout(oldCanonical)
	}
	o.removeWhiteout(newpath)
	return 0
}

func (o *OverlayFS) Symlink(target string, newpath string) int {
	if o.upperDir == "" {
		return -fuse.EROFS
	}
	up := o.upper(newpath)
	if err := os.MkdirAll(filepath.Dir(up), 0755); err != nil {
		return -fuse.EIO
	}
	o.removeWhiteout(newpath)
	if err := os.Symlink(target, up); err != nil {
		return -fuse.EIO
	}
	return 0
}

func (o *OverlayFS) Readlink(path string) (int, string) {
	// Check upper first.
	if target, err := os.Readlink(o.upper(path)); err == nil {
		return 0, target
	}
	// Fall back to squashfs: the symlink target is stored as file content.
	target, err := o.squash.ReadSymlink(path)
	if err != nil {
		return -fuse.ENOENT, ""
	}
	return 0, target
}

// --- File open / read / write --------------------------------------------

func (o *OverlayFS) Open(path string, flags int) (int, uint64) {
	isWrite := flags&(os.O_WRONLY|os.O_RDWR) != 0
	up := o.upper(path)

	// Upper layer: open directly.
	if _, err := os.Lstat(up); err == nil {
		f, err := os.OpenFile(up, flags, 0644)
		if err != nil {
			return -fuse.EACCES, invalidFH
		}
		h := &handle{}
		h.upperFile.Store(f)
		return 0, o.storeHandle(h)
	}

	// Whiteout check.
	if o.hasWhiteout(path) {
		return -fuse.ENOENT, invalidFH
	}

	// Squashfs layer.
	if isWrite {
		if o.upperDir == "" {
			return -fuse.EROFS, invalidFH
		}
		// Copy-on-write: materialise the file in upper before opening for write.
		if err := o.copySquashToUpper(path); err != nil {
			return -fuse.EIO, invalidFH
		}
		f, err := os.OpenFile(up, flags, 0644)
		if err != nil {
			return -fuse.EIO, invalidFH
		}
		h := &handle{}
		h.upperFile.Store(f)
		return 0, o.storeHandle(h)
	}

	// Read-only from squashfs: open and store the fs.File (implements io.ReaderAt).
	// No buffering — KarpelesLab/squashfs decompresses only the requested block per Read.
	sqf, err := o.squash.Open(path)
	if err != nil {
		return -fuse.ENOENT, invalidFH
	}
	return 0, o.storeHandle(&handle{sqFile: sqf})
}

func (o *OverlayFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	h, ok := o.loadHandle(fh)
	if !ok {
		return -fuse.EBADF
	}

	if uf := h.upperFile.Load(); uf != nil {
		n, err := uf.ReadAt(buff, ofst)
		if err != nil && err != io.EOF {
			return -fuse.EIO
		}
		return n
	}

	// Squashfs: use io.ReaderAt for per-block random access (no full-file buffer).
	if ra, ok := h.sqFile.(io.ReaderAt); ok {
		n, err := ra.ReadAt(buff, ofst)
		if err != nil && err != io.EOF {
			return -fuse.EIO
		}
		return n
	}
	return -fuse.EIO
}

func (o *OverlayFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	h, ok := o.loadHandle(fh)
	if !ok {
		return -fuse.EBADF
	}

	// upperFile is always set for write-mode handles (Open CoWs eagerly).
	// The VFS rejects write(2) on O_RDONLY fds before reaching FUSE, so
	// upperFile == nil here is impossible in normal operation.
	uf := h.upperFile.Load()
	if uf == nil {
		return -fuse.EBADF
	}

	n, err := uf.WriteAt(buff, ofst)
	if err != nil {
		return -fuse.EIO
	}
	return n
}

func (o *OverlayFS) Truncate(path string, size int64, fh uint64) int {
	if o.upperDir == "" {
		return -fuse.EROFS
	}
	// Fast path via open handle.
	if fh != invalidFH {
		if h, ok := o.loadHandle(fh); ok {
			if uf := h.upperFile.Load(); uf != nil {
				return errToFuse(uf.Truncate(size))
			}
		}
	}

	up := o.upper(path)
	if _, err := os.Lstat(up); err != nil {
		// Not in upper yet — CoW first.
		if err2 := o.copySquashToUpper(path); err2 != nil {
			return -fuse.EIO
		}
	}
	return errToFuse(os.Truncate(up, size))
}

func (o *OverlayFS) Flush(path string, fh uint64) int  { return 0 }
func (o *OverlayFS) Fsync(path string, datasync bool, fh uint64) int {
	if h, ok := o.loadHandle(fh); ok {
		if uf := h.upperFile.Load(); uf != nil {
			return errToFuse(uf.Sync())
		}
	}
	return 0
}

func (o *OverlayFS) Release(path string, fh uint64) int {
	v, ok := o.fhs.LoadAndDelete(fh)
	if ok {
		h := v.(*handle)
		if uf := h.upperFile.Load(); uf != nil {
			uf.Close()
		}
		if h.sqFile != nil {
			h.sqFile.Close()
		}
	}
	return 0
}

func (o *OverlayFS) Releasedir(path string, fh uint64) int { return 0 }

// --- Internal helpers ----------------------------------------------------

// upper converts a FUSE absolute path to the corresponding OS path inside
// the upper directory. On Linux, FUSE always provides clean absolute paths
// (e.g. "/dir/file"), so direct concatenation is safe and avoids the
// filepath.Join alloc + path.Clean scan.
//
// In read-only mode (no upper dir) it returns "", which every os.* call
// rejects; concatenating "" + "/etc" would otherwise expose the host root.
func (o *OverlayFS) upper(fusePath string) string {
	if o.upperDir == "" {
		return ""
	}
	return o.upperDir + fusePath
}

// whiteoutPath returns the OS path of the whiteout marker for fusePath.
// fusePath is a clean absolute FUSE path (e.g. "/dir/file").
// Returns "" in read-only mode (see upper).
func (o *OverlayFS) whiteoutPath(fusePath string) string {
	if o.upperDir == "" {
		return ""
	}
	i := strings.LastIndexByte(fusePath, '/')
	// fusePath[:i] is the parent dir portion (may be "" for root-level entries).
	// fusePath[i+1:] is the base name.
	return o.upperDir + fusePath[:i] + "/" + whiteoutPrefix + fusePath[i+1:]
}

// hasWhiteout reports whether fusePath has been deleted in the upper layer.
// It checks both the exact path and the squashfs-canonical path to handle the
// case where the whiteout was created with correct squashfs casing but the
// current access uses a different case.
func (o *OverlayFS) hasWhiteout(fusePath string) bool {
	if fusePath == "/" {
		return false
	}
	if _, err := os.Lstat(o.whiteoutPath(fusePath)); err == nil {
		return true
	}
	// Try the squashfs-resolved casing (whiteouts are created with canonical case).
	if canonical := o.squash.ResolvePath(fusePath); canonical != fusePath {
		_, err := os.Lstat(o.whiteoutPath(canonical))
		return err == nil
	}
	return false
}

func (o *OverlayFS) createWhiteout(fusePath string) {
	wp := o.whiteoutPath(fusePath)
	_ = os.MkdirAll(wp[:strings.LastIndexByte(wp, '/')], 0755)
	if f, err := os.Create(wp); err == nil {
		f.Close()
	}
}

// removeWhiteout removes any whiteout for fusePath. It removes both the
// exact-case and squashfs-canonical-case whiteout files, since either may
// exist depending on how the file was previously deleted.
func (o *OverlayFS) removeWhiteout(fusePath string) {
	_ = os.Remove(o.whiteoutPath(fusePath))
	if canonical := o.squash.ResolvePath(fusePath); canonical != fusePath {
		_ = os.Remove(o.whiteoutPath(canonical))
	}
}

// copySquashToUpper copies fusePath from squashfs into the upper directory,
// preserving the file's permission bits.  Parent directories are created
// as needed.
func (o *OverlayFS) copySquashToUpper(fusePath string) error {
	return o.copySquashToUpperAt(fusePath, fusePath)
}

func (o *OverlayFS) copySquashToUpperAt(sqFusePath, destFusePath string) error {
	// Use KarpelesLab Stat for permission bits (it's already open).
	info, err := o.squash.Stat(sqFusePath)
	if err != nil {
		return err
	}

	up := o.upper(destFusePath)
	if err := os.MkdirAll(filepath.Dir(up), 0755); err != nil {
		return err
	}

	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0644
	}
	dst, err := os.OpenFile(up, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer dst.Close()

	// CalebQ42 parallel WriteTo: decompresses blocks concurrently with a
	// sync.Pool — far cheaper in allocations than KarpelesLab's per-block alloc.
	return o.squash.CopyTo(sqFusePath, dst)
}

func (o *OverlayFS) storeHandle(h *handle) uint64 {
	fh := o.nextFH.Add(1)
	o.fhs.Store(fh, h)
	return fh
}

func (o *OverlayFS) loadHandle(fh uint64) (*handle, bool) {
	v, ok := o.fhs.Load(fh)
	if !ok {
		return nil, false
	}
	return v.(*handle), true
}

// --- Stat helpers --------------------------------------------------------

// fillStat converts fs.FileInfo (from either os.Lstat or squashfs) into
// cgofuse's POSIX-style Stat_t.
func fillStat(stat *fuse.Stat_t, info fs.FileInfo) {
	stat.Nlink = 1
	stat.Uid = 0
	stat.Gid = 0

	t := fuse.Timespec{
		Sec:  info.ModTime().Unix(),
		Nsec: int64(info.ModTime().Nanosecond()),
	}
	stat.Atim = t
	stat.Mtim = t
	stat.Ctim = t

	mode := info.Mode()
	perm := uint32(mode.Perm())

	switch {
	case mode.IsDir():
		if perm == 0 {
			perm = 0755
		}
		stat.Mode = fuse.S_IFDIR | perm
		stat.Size = 0
	case mode&fs.ModeSymlink != 0:
		stat.Mode = fuse.S_IFLNK | 0777
		stat.Size = info.Size()
	default:
		if perm == 0 {
			perm = 0644
		}
		stat.Mode = fuse.S_IFREG | perm
		stat.Size = info.Size()
	}
}

// errToFuse maps a Go error to a negative fuse errno.
func errToFuse(err error) int {
	if err == nil {
		return 0
	}
	if os.IsNotExist(err) {
		return -fuse.ENOENT
	}
	if os.IsPermission(err) {
		return -fuse.EACCES
	}
	if os.IsExist(err) {
		return -fuse.EEXIST
	}
	return -fuse.EIO
}
