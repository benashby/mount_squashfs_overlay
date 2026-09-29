//go:build windows

package main

// This file implements the gofs.FileSystem and gofs.File interfaces
// from go-winfsp, providing an overlay filesystem over a squashfs
// lower layer and a writable upper directory.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/winfsp/go-winfsp/gofs"
)

// Whiteout conventions (compatible with Docker/OCI overlayfs):
//
//	.wh.<name>        — marks <name> as deleted in the lower (squashfs) layer
//	.wh..wh..opq      — marks the directory as opaque (hides all lower-layer contents)
//	.wh..wh..redirect — the directory's lower-layer contents live at the
//	                    squashfs path stored in this file (see Rename)
const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
	redirectMarker = ".wh..wh..redirect"
)

// OverlayFileSystem implements gofs.FileSystem, presenting a union of:
//   - lower layer: a read-only squashfs archive
//   - upper layer: a writable directory on disk (persistent)
//
// Reads check the upper directory first; writes always go to the upper
// directory. Deletions are stored as whiteout marker files.
type OverlayFileSystem struct {
	squash   *SquashLayer
	upperDir string
	debug    bool

	// Renamed directories: lowercased overlay path ("/a/b") -> squashfs
	// path of their lower-layer contents. Loaded from the redirect
	// markers in the upper dir on first use.
	redirOnce sync.Once
	redirMu   sync.RWMutex
	redirects map[string]string
}

func (ofs *OverlayFileSystem) log(format string, v ...any) {
	if ofs.debug {
		fmt.Fprintf(os.Stderr, format+"\n", v...)
	}
}

func (ofs *OverlayFileSystem) logErr(op, name string, err error) error {
	if ofs.debug {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %q -> ERROR: %v\n", op, name, err)
		} else {
			fmt.Fprintf(os.Stderr, "%s: %q -> ok\n", op, name)
		}
	}
	return err
}

var _ gofs.FileSystem = (*OverlayFileSystem)(nil)

// upper converts an overlay path (forward-slash, rooted) to an OS path
// inside the upper directory.
//
// In read-only mode (no upper dir) it returns "", which every os.Stat/Lstat/
// ReadDir/Open call rejects. Joining "" with a rooted name would otherwise
// give a path like `\Windows`, which resolves against the root of the
// process's current drive and leaks that drive's contents into the mount.
func (ofs *OverlayFileSystem) upper(name string) string {
	if ofs.upperDir == "" {
		return ""
	}
	return filepath.Join(ofs.upperDir, filepath.FromSlash(name))
}

// ── Renamed directories ───────────────────────────────────────────────
//
// Renaming a directory that has lower-layer contents does not copy them.
// The directory is created (or moved) in the upper layer with a redirect
// marker holding the squashfs path of its contents, and the old name gets
// a whiteout. lower() maps overlay paths through those redirects before
// every squashfs lookup, so the contents appear under the new name.

// cleanOverlayPath returns name as a rooted, forward-slash path.
func cleanOverlayPath(name string) string {
	return path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
}

func (ofs *OverlayFileSystem) loadRedirects() {
	ofs.redirects = map[string]string{}
	if ofs.upperDir == "" {
		return
	}
	filepath.WalkDir(ofs.upperDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != redirectMarker {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(ofs.upperDir, filepath.Dir(p))
		if err != nil {
			return nil
		}
		ofs.redirects[strings.ToLower(cleanOverlayPath(rel))] = cleanOverlayPath(strings.TrimSpace(string(b)))
		return nil
	})
	ofs.log("loaded %d directory redirects", len(ofs.redirects))
}

// lower maps an overlay path to the squashfs path holding its lower-layer
// contents: the path itself, or a path under the nearest renamed ancestor's
// original location.
func (ofs *OverlayFileSystem) lower(name string) string {
	p := cleanOverlayPath(name)
	ofs.redirOnce.Do(ofs.loadRedirects)
	ofs.redirMu.RLock()
	defer ofs.redirMu.RUnlock()
	if len(ofs.redirects) == 0 || p == "/" {
		return p
	}
	parts := strings.Split(p[1:], "/")
	base, rest := "", -1
	prefix := ""
	for i, part := range parts {
		prefix += "/" + part
		if l, ok := ofs.redirects[strings.ToLower(prefix)]; ok {
			base, rest = l, i+1
		}
	}
	if rest < 0 {
		return p
	}
	return path.Join(append([]string{base}, parts[rest:]...)...)
}

// moveRedirects updates the redirect map after an upper-layer directory
// moved from source to target, or was removed (target == "").
func (ofs *OverlayFileSystem) moveRedirects(source, target string) {
	ofs.redirOnce.Do(ofs.loadRedirects)
	src := strings.ToLower(cleanOverlayPath(source))
	ofs.redirMu.Lock()
	defer ofs.redirMu.Unlock()
	for k, v := range ofs.redirects {
		if k != src && !strings.HasPrefix(k, src+"/") {
			continue
		}
		delete(ofs.redirects, k)
		if target != "" {
			ofs.redirects[strings.ToLower(cleanOverlayPath(target))+k[len(src):]] = v
		}
	}
}

// addRedirect writes a redirect marker into the upper-layer directory at
// name, pointing at the squashfs path lowerPath.
func (ofs *OverlayFileSystem) addRedirect(name, lowerPath string) error {
	marker := filepath.Join(ofs.upper(name), redirectMarker)
	if err := os.WriteFile(marker, []byte(lowerPath+"\n"), 0o644); err != nil {
		return err
	}
	ofs.redirOnce.Do(ofs.loadRedirects)
	ofs.redirMu.Lock()
	ofs.redirects[strings.ToLower(cleanOverlayPath(name))] = lowerPath
	ofs.redirMu.Unlock()
	return nil
}

// ── FileSystem interface ──────────────────────────────────────────────

func (ofs *OverlayFileSystem) Stat(name string) (os.FileInfo, error) {
	ofs.log("Stat: %q", name)
	up := ofs.upper(name)
	if info, err := os.Stat(up); err == nil {
		ofs.log("Stat: %q -> upper ok, size=%d", name, info.Size())
		return info, nil
	}
	if ofs.lowerHidden(name) {
		ofs.log("Stat: %q -> has whiteout", name)
		return nil, os.ErrNotExist
	}
	info, err := ofs.squash.Stat(ofs.lower(name))
	if err != nil {
		ofs.log("Stat: %q -> squash err: %v", name, err)
	} else {
		ofs.log("Stat: %q -> squash ok, isDir: %v, size: %d", name, info.IsDir(), info.Size())
	}
	return info, err
}

func (ofs *OverlayFileSystem) OpenFile(name string, flag int, perm os.FileMode) (gofs.File, error) {
	ofs.log("OpenFile: %q flag=%d", name, flag)
	isWrite := flag&(os.O_WRONLY|os.O_RDWR|os.O_TRUNC) != 0
	if isWrite && ofs.upperDir == "" {
		return nil, os.ErrPermission
	}
	up := ofs.upper(name)

	// ── Upper layer ──
	if info, err := os.Lstat(up); err == nil {
		ofs.log("OpenFile: %q exists in upper layer", name)
		if info.IsDir() {
			// For directories, we return a file object whose Readdir
			// merges upper + squashfs entries.
			f, err := os.Open(up)
			if err != nil {
				return nil, err
			}
			return &overlayFile{ofs: ofs, path: name, upper: f, isDir: true}, nil
		}
		// File exists in upper layer, use it
		f, err := os.OpenFile(up, flag, perm)
		if err != nil {
			return nil, err
		}
		return &overlayFile{ofs: ofs, path: name, upper: f}, nil
	}

	// ── Whiteout check ──
	// O_CREATE means the caller wants to create a fresh file; the whiteout
	// should be cleared rather than blocking the open.
	if ofs.lowerHidden(name) {
		if flag&os.O_CREATE == 0 {
			ofs.log("OpenFile: %q is whiteout", name)
			return nil, os.ErrNotExist
		}
		ofs.log("OpenFile: %q has whiteout but O_CREATE set, will clear it", name)
	}

	// ── Squashfs layer ──
	sqInfo, err := ofs.squash.Stat(ofs.lower(name))
	if err == nil && ofs.lowerHidden(name) {
		err = os.ErrNotExist
	}
	if err != nil {
		ofs.log("OpenFile squash.Stat err: %v", err)
		// File doesn't exist in either layer (or is whited-out).
		// Only create if O_CREATE is explicitly set.
		if flag&os.O_CREATE != 0 {
			if ofs.upperDir == "" {
				return nil, os.ErrPermission
			}
			ofs.log("OpenFile: %q creating new file in upper", name)
			if err := os.MkdirAll(filepath.Dir(up), 0755); err != nil {
				return nil, err
			}
			ofs.removeWhiteout(name)
			f, err := os.OpenFile(up, flag, perm)
			if err != nil {
				return nil, err
			}
			return &overlayFile{ofs: ofs, path: name, upper: f}, nil
		}
		return nil, os.ErrNotExist
	}

	if sqInfo.IsDir() {
		// Pure squashfs directory (not in upper at all).
		ofs.log("OpenFile: %q is squashfs directory", name)
		return &overlayFile{ofs: ofs, path: name, isDir: true, sqDirInfo: sqInfo}, nil
	}

	// Regular squashfs file.
	ofs.log("OpenFile: %q is squashfs file, size=%d", name, sqInfo.Size())
	if isWrite || (flag&os.O_TRUNC != 0) {
		// Copy-on-write: materialise in upper first.
		ofs.log("OpenFile: %q triggering CoW (isWrite=%v, O_TRUNC=%v)", name, isWrite, flag&os.O_TRUNC != 0)
		if err := ofs.copySquashToUpper(name); err != nil {
			ofs.log("OpenFile: %q CoW copy failed: %v", name, err)
			return nil, err
		}
		// Clear any stale whiteout now that the file is materialised in upper.
		ofs.removeWhiteout(name)
		f, err := os.OpenFile(up, flag, perm)
		if err != nil {
			return nil, err
		}
		return &overlayFile{ofs: ofs, path: name, upper: f}, nil
	}

	// Read-only squashfs file.
	sqf, err := ofs.squash.Open(ofs.lower(name))
	if err != nil {
		ofs.log("OpenFile squash.Open err: %v", err)
		return nil, err
	}
	ofs.log("OpenFile: %q opened from squashfs with size=%d", name, sqInfo.Size())
	return &overlayFile{ofs: ofs, path: name, sqFile: sqf, sqInfo: sqInfo}, nil
}

func (ofs *OverlayFileSystem) Mkdir(name string, perm os.FileMode) error {
	ofs.log("Mkdir: %q perm=%o", name, perm)
	if ofs.upperDir == "" {
		return ofs.logErr("Mkdir", name, os.ErrPermission)
	}
	up := ofs.upper(name)
	hidden := ofs.lowerHidden(name)
	if err := os.MkdirAll(up, perm|0111); err != nil {
		return ofs.logErr("Mkdir", name, err)
	}
	ofs.removeWhiteout(name)
	// A directory made where a deleted one was starts empty.
	if hidden {
		if info, err := ofs.squash.Stat(ofs.lower(name)); err == nil && info.IsDir() {
			ofs.makeOpaque(name)
		}
	}
	return ofs.logErr("Mkdir", name, nil)
}

func (ofs *OverlayFileSystem) Remove(name string) error {
	ofs.log("Remove: %q", name)
	if ofs.upperDir == "" {
		return ofs.logErr("Remove", name, os.ErrPermission)
	}

	up := ofs.upper(name)
	// Remove from upper if present.
	info, upperErr := os.Lstat(up)
	if upperErr == nil {
		if info.IsDir() {
			if err := os.RemoveAll(up); err != nil {
				return ofs.logErr("Remove", name, err)
			}
			ofs.log("Remove: %q removed from upper (dir)", name)
		} else {
			if err := os.Remove(up); err != nil {
				return ofs.logErr("Remove", name, err)
			}
			ofs.log("Remove: %q removed from upper (file)", name)
		}
	} else {
		ofs.log("Remove: %q not in upper (%v)", name, upperErr)
	}
	if upperErr == nil && info.IsDir() {
		ofs.moveRedirects(name, "")
	}
	// If the entry exists in squashfs, plant a whiteout.
	if _, err := ofs.squash.Stat(ofs.lower(name)); err == nil {
		ofs.log("Remove: %q exists in squashfs, creating whiteout", name)
		ofs.createWhiteout(name)
	}
	return ofs.logErr("Remove", name, nil)
}

func (ofs *OverlayFileSystem) Rename(source, target string) error {
	ofs.log("Rename: %q -> %q", source, target)
	if ofs.upperDir == "" {
		return ofs.logErr("Rename", source, os.ErrPermission)
	}
	srcUp := ofs.upper(source)
	dstUp := ofs.upper(target)

	if err := os.MkdirAll(filepath.Dir(dstUp), 0755); err != nil {
		return ofs.logErr("Rename", source, err)
	}

	existsInUpper := false
	upInfo, err := os.Lstat(srcUp)
	if err == nil {
		existsInUpper = true
	}
	ofs.log("Rename: %q existsInUpper=%v", source, existsInUpper)

	// Lower-layer contents of the source, found before anything moves.
	srcLower := ofs.lower(source)
	targetHidden := ofs.lowerHidden(target)
	sqInfo, sqErr := ofs.squash.Stat(srcLower)
	if !existsInUpper && sqErr != nil {
		return ofs.logErr("Rename", source, os.ErrNotExist)
	}
	isDir := (existsInUpper && upInfo.IsDir()) || (!existsInUpper && sqInfo.IsDir())

	switch {
	case isDir:
		// Move the upper part, if any, and point the new name at the
		// lower contents instead of copying them.
		if existsInUpper {
			if err := os.Rename(srcUp, dstUp); err != nil {
				return ofs.logErr("Rename", source, err)
			}
			ofs.moveRedirects(source, target)
		} else if err := os.MkdirAll(dstUp, 0755); err != nil {
			return ofs.logErr("Rename", source, err)
		}
		_, markerErr := os.Lstat(filepath.Join(dstUp, redirectMarker))
		_, opaqueErr := os.Lstat(filepath.Join(dstUp, opaqueWhiteout))
		redirected := markerErr == nil
		if sqErr == nil && sqInfo.IsDir() && !ofs.lowerHidden(source) && markerErr != nil && opaqueErr != nil {
			if err := ofs.addRedirect(target, srcLower); err != nil {
				return ofs.logErr("Rename", source, err)
			}
			redirected = true
		}
		// Landing on a deleted name must not bring its old contents back.
		if !redirected && opaqueErr != nil && targetHidden {
			if info, err := ofs.squash.Stat(ofs.lower(target)); err == nil && info.IsDir() {
				ofs.makeOpaque(target)
			}
		}
	case existsInUpper:
		if err := os.Rename(srcUp, dstUp); err != nil {
			return ofs.logErr("Rename", source, err)
		}
	default:
		// File only in squashfs: CoW it to the new upper location.
		if err := ofs.copySquashToUpperAt(srcLower, target); err != nil {
			return ofs.logErr("Rename", source, err)
		}
	}

	// If lower-layer contents still show at the old name, hide them.
	if _, err := ofs.squash.Stat(ofs.lower(source)); err == nil {
		ofs.log("Rename: source %q exists in squashfs, creating whiteout", source)
		ofs.createWhiteout(source)
	}
	ofs.removeWhiteout(target)
	return ofs.logErr("Rename", source, nil)
}

// ── Whiteout helpers ──────────────────────────────────────────────────

// lowerHidden reports whether the lower layer is hidden at name: by a
// whiteout of name or of any of its parents, or by an opaque parent. It
// walks the upper dir from the root and stops at the first component
// that has no upper directory, since nothing deeper can exist there.
func (ofs *OverlayFileSystem) lowerHidden(name string) bool {
	p := cleanOverlayPath(name)
	if ofs.upperDir == "" || p == "/" {
		return false
	}
	parts := strings.Split(p[1:], "/")
	dir := ofs.upperDir
	for i, part := range parts {
		if _, err := os.Lstat(filepath.Join(dir, whiteoutPrefix+part)); err == nil {
			return true
		}
		if i == len(parts)-1 {
			return false
		}
		dir = filepath.Join(dir, part)
		if _, err := os.Lstat(filepath.Join(dir, opaqueWhiteout)); err == nil {
			return true
		}
		if _, err := os.Lstat(dir); err != nil {
			return false
		}
	}
	return false
}

// makeOpaque marks the upper directory at name as hiding all lower-layer
// contents at that path.
func (ofs *OverlayFileSystem) makeOpaque(name string) {
	if f, err := os.Create(filepath.Join(ofs.upper(name), opaqueWhiteout)); err == nil {
		f.Close()
	}
}

func (ofs *OverlayFileSystem) createWhiteout(name string) {
	if ofs.upperDir == "" {
		return
	}
	dir := filepath.Dir(name)
	base := filepath.Base(name)
	wp := filepath.Join(ofs.upperDir, filepath.FromSlash(dir), whiteoutPrefix+base)
	_ = os.MkdirAll(filepath.Dir(wp), 0755)
	if f, err := os.Create(wp); err == nil {
		f.Close()
	}
}

func (ofs *OverlayFileSystem) removeWhiteout(name string) {
	if ofs.upperDir == "" {
		return
	}
	dir := filepath.Dir(name)
	base := filepath.Base(name)
	wp := filepath.Join(ofs.upperDir, filepath.FromSlash(dir), whiteoutPrefix+base)
	_ = os.Remove(wp)
}

// ── CoW helpers ───────────────────────────────────────────────────────

func (ofs *OverlayFileSystem) copySquashToUpper(name string) error {
	return ofs.copySquashToUpperAt(ofs.lower(name), name)
}

func (ofs *OverlayFileSystem) copySquashToUpperAt(sqPath, destPath string) error {
	info, err := ofs.squash.Stat(sqPath)
	if err != nil {
		return err
	}
	up := ofs.upper(destPath)
	if err := os.MkdirAll(filepath.Dir(up), 0755); err != nil {
		return err
	}
	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0644
	}
	
	// Use a temporary file to avoid leaving 0-byte files if copy fails
	tmpUp := up + ".tmp"
	
	// Clean up any stale temp files
	_ = os.Remove(tmpUp)
	
	// Create and copy to temp file
	dst, err := os.OpenFile(tmpUp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	
	copyErr := ofs.squash.CopyTo(sqPath, dst)
	dst.Close()
	
	if copyErr != nil {
		// Clean up the temp file if copy failed
		ofs.log("copySquashToUpperAt: %q copy failed, cleaning up temp file: %v", sqPath, copyErr)
		_ = os.Remove(tmpUp)
		return copyErr
	}
	
	// Move temp file to final location
	if err := os.Rename(tmpUp, up); err != nil {
		_ = os.Remove(tmpUp)
		return err
	}
	
	ofs.log("copySquashToUpperAt: %q successfully copied to upper layer", sqPath)
	return nil
}

// ── Directory merging ─────────────────────────────────────────────────

// mergeDir returns a sorted, deduplicated list of os.FileInfo entries
// for the given overlay directory, merging upper and squashfs layers
// while respecting whiteouts and opaque markers.
func (ofs *OverlayFileSystem) mergeDir(path string) []os.FileInfo {
	seen := make(map[string]bool)
	whiteouts := make(map[string]bool)
	opaque := false
	var result []os.FileInfo

	// Precompute a normalised forward-slash prefix for this directory once,
	// used both for the skip-list check and for building child paths.
	normPrefix := strings.TrimRight(strings.ReplaceAll(path, "\\", "/"), "/")

	// Upper layer entries.
	upPath := ofs.upper(path)
	if des, err := os.ReadDir(upPath); err == nil {
		for _, de := range des {
			name := de.Name()
			childPath := normPrefix + "/" + name
			if strings.EqualFold(childPath, "/dosdevices") ||
				strings.EqualFold(childPath, "/drive_c/windows") ||
				strings.EqualFold(childPath, "/.update-timestamp") ||
				strings.EqualFold(childPath, "/system.reg") ||
				strings.EqualFold(childPath, "/user.reg") ||
				strings.EqualFold(childPath, "/userdef.reg") {
				continue
			}
			switch {
			case name == opaqueWhiteout:
				opaque = true
			case strings.HasPrefix(name, whiteoutPrefix):
				// Lowercase so a whiteout for "config.ini" suppresses squashfs
				// entry "Config.ini" — Windows apps may use any casing.
				whiteouts[strings.ToLower(name[len(whiteoutPrefix):])] = true
			default:
				if info, err := de.Info(); err == nil {
					result = append(result, info)
					// Lowercase key: upper "Config.ini" must suppress squashfs
					// "config.ini" (same logical file on case-insensitive Windows).
					seen[strings.ToLower(name)] = true
				}
			}
		}
	}

	// Lower (squashfs) layer entries — unless opaque or deleted.
	if !opaque && !ofs.lowerHidden(path) {
		if sqDes, err := ofs.squash.ReadDir(ofs.lower(path)); err == nil {
			ofs.log("mergeDir(%q) ReadDir got %d entries", path, len(sqDes))
			for _, de := range sqDes {
				name := de.Name()
				childPath := normPrefix + "/" + name
				if strings.EqualFold(childPath, "/dosdevices") ||
					strings.EqualFold(childPath, "/drive_c/windows") ||
					strings.EqualFold(childPath, "/.update-timestamp") ||
					strings.EqualFold(childPath, "/system.reg") ||
					strings.EqualFold(childPath, "/user.reg") ||
					strings.EqualFold(childPath, "/userdef.reg") {
					continue
				}
				if seen[strings.ToLower(name)] || whiteouts[strings.ToLower(name)] {
					continue
				}
				// de.Info() returns the DirEntry's cached FileInfo at no extra
				// cost — avoids a full squashfs inode lookup + symlink follow
				// (squash.Stat) for every entry in the directory listing.
				if info, err := de.Info(); err == nil {
					result = append(result, info)
					seen[strings.ToLower(name)] = true
				} else {
					ofs.log("mergeDir de.Info %q error: %v", childPath, err)
				}
			}
		} else {
			ofs.log("mergeDir(%q) squash.ReadDir error: %v", path, err)
		}
	}

	// Sort for consistent ordering.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name() < result[j].Name()
	})
	ofs.log("mergeDir(%q) returning %d entries", path, len(result))
	return result
}

// ── overlayFile (gofs.File) ───────────────────────────────────────────

// overlayFile implements gofs.File for both regular files and directories
// in the overlay filesystem.
type overlayFile struct {
	ofs  *OverlayFileSystem
	path string

	isDir bool

	// Regular file handles (mutually exclusive at any given time).
	upper  *os.File // non-nil when file is in upper dir
	sqFile fs.File  // non-nil for read-only squashfs files
	sqInfo fs.FileInfo
	pos    int64 // read position tracking for squashfs files

	// Directory-only: info when dir exists only in squashfs.
	sqDirInfo fs.FileInfo

	// Directory listing state.
	dirEntries []os.FileInfo
	dirPos     int
}

var _ gofs.File = (*overlayFile)(nil)

func (f *overlayFile) Read(p []byte) (int, error) {
	if f.upper != nil {
		return f.upper.Read(p)
	}
	if f.sqFile == nil {
		return 0, io.EOF
	}
	if ra, ok := f.sqFile.(io.ReaderAt); ok {
		n, err := ra.ReadAt(p, f.pos)
		f.pos += int64(n)
		return n, err
	}
	return 0, io.ErrUnexpectedEOF
}

func (f *overlayFile) ReadAt(p []byte, off int64) (int, error) {
	if f.upper != nil {
		return f.upper.ReadAt(p, off)
	}
	if f.sqFile == nil {
		return 0, io.EOF
	}
	if ra, ok := f.sqFile.(io.ReaderAt); ok {
		return ra.ReadAt(p, off)
	}
	return 0, io.ErrUnexpectedEOF
}

func (f *overlayFile) Write(p []byte) (int, error) {
	if f.upper != nil {
		return f.upper.Write(p)
	}
	if err := f.cowToUpper(); err != nil {
		return 0, err
	}
	return f.upper.Write(p)
}

func (f *overlayFile) WriteAt(p []byte, off int64) (int, error) {
	if f.upper != nil {
		return f.upper.WriteAt(p, off)
	}
	if err := f.cowToUpper(); err != nil {
		return 0, err
	}
	return f.upper.WriteAt(p, off)
}

func (f *overlayFile) Seek(offset int64, whence int) (int64, error) {
	if f.upper != nil {
		return f.upper.Seek(offset, whence)
	}
	// For squashfs, track position manually.
	var size int64
	if f.sqInfo != nil {
		size = f.sqInfo.Size()
	}
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = f.pos + offset
	case io.SeekEnd:
		newPos = size + offset
	default:
		return 0, os.ErrInvalid
	}
	if newPos < 0 {
		return 0, os.ErrInvalid
	}
	f.pos = newPos
	return newPos, nil
}

func (f *overlayFile) Close() error {
	if f.upper != nil {
		return f.upper.Close()
	}
	if f.sqFile != nil {
		return f.sqFile.Close()
	}
	return nil
}

func (f *overlayFile) Stat() (os.FileInfo, error) {
	f.ofs.log("overlayFile.Stat(%q)", f.path)
	if f.upper != nil {
		info, err := f.upper.Stat()
		if err == nil {
			f.ofs.log("overlayFile.Stat(%q) -> upper, size=%d", f.path, info.Size())
		}
		return info, err
	}
	if f.sqInfo != nil {
		f.ofs.log("overlayFile.Stat(%q) -> sqInfo, size=%d", f.path, f.sqInfo.Size())
		return f.sqInfo, nil
	}
	if f.sqDirInfo != nil {
		f.ofs.log("overlayFile.Stat(%q) -> sqDirInfo", f.path)
		return f.sqDirInfo, nil
	}
	return f.ofs.Stat(f.path)
}

func (f *overlayFile) Sync() error {
	if f.upper != nil {
		return f.upper.Sync()
	}
	return nil
}

func (f *overlayFile) Truncate(size int64) error {
	if f.upper != nil {
		return f.upper.Truncate(size)
	}
	if err := f.cowToUpper(); err != nil {
		return err
	}
	return f.upper.Truncate(size)
}

// Readdir returns merged overlay directory entries. Called by gofs
// with count=-1 to get all entries at once.
func (f *overlayFile) Readdir(count int) ([]os.FileInfo, error) {
	f.ofs.log("overlayFile.Readdir(%q, count=%d)", f.path, count)
	if !f.isDir {
		f.ofs.log("overlayFile.Readdir(%q) error: not a dir", f.path)
		return nil, os.ErrInvalid
	}

	// Build merged entry list on first call.
	if f.dirEntries == nil {
		f.dirEntries = f.ofs.mergeDir(f.path)
		f.dirPos = 0
	}

	if count <= 0 {
		// Return all remaining entries. No io.EOF when count <= 0.
		entries := f.dirEntries[f.dirPos:]
		f.dirPos = len(f.dirEntries)
		f.ofs.log("overlayFile.Readdir(%q) returning %d entries", f.path, len(entries))
		return entries, nil
	}

	// Return up to count entries.
	if f.dirPos >= len(f.dirEntries) {
		return nil, io.EOF
	}
	remaining := len(f.dirEntries) - f.dirPos
	n := count
	if n > remaining {
		n = remaining
	}
	entries := f.dirEntries[f.dirPos : f.dirPos+n]
	f.dirPos += n
	return entries, nil
}

// cowToUpper copies the squashfs file to the upper directory and
// switches this handle to use the upper file for subsequent I/O.
func (f *overlayFile) cowToUpper() error {
	if f.ofs.upperDir == "" {
		return os.ErrPermission
	}
	if f.sqFile != nil {
		f.sqFile.Close()
		f.sqFile = nil
	}
	if err := f.ofs.copySquashToUpper(f.path); err != nil {
		return err
	}
	uf, err := os.OpenFile(f.ofs.upper(f.path), os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	if f.pos > 0 {
		uf.Seek(f.pos, io.SeekStart)
	}
	f.upper = uf
	f.sqInfo = nil
	return nil
}
