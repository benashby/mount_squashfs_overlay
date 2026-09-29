//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/gofs"
	"github.com/winfsp/go-winfsp/pathlock"
	"golang.org/x/sys/windows"
)

// mountFixture mounts the fixture image with an overlay in a temp dir on a
// free drive letter and returns the drive root ("X:\") and the overlay dir.
// It skips the test when WinFsp is not installed.
func mountFixture(t *testing.T) (root, upper string) {
	t.Helper()
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	letter := byte(0)
	for c := byte('Z'); c >= 'H'; c-- {
		if mask&(1<<(c-'A')) == 0 {
			letter = c
			break
		}
	}
	if letter == 0 {
		t.Skip("no free drive letter")
	}
	upper = t.TempDir()
	ofs := &OverlayFileSystem{squash: openFixture(t), upperDir: upper}
	drive := string(letter) + ":"
	ptfs, err := winfsp.Mount(gofs.New(ofs), drive)
	if err != nil {
		t.Skipf("WinFsp mount unavailable: %v", err)
	}
	t.Cleanup(ptfs.Unmount)
	return drive + `\`, upper
}

// openShared opens path the way Explorer does: sharing read, write and
// delete with other handles.
func openShared(t *testing.T, path string, access uint32) windows.Handle {
	t.Helper()
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("CreateFile(%s, %#x): %v", path, access, err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

func tryOpen(path string, access uint32) error {
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		windows.CloseHandle(h)
	}
	return err
}

func TestPathLockTryUpgrade(t *testing.T) {
	var l pathlock.PathLocker
	a := l.RLockPath("/dir/file")
	if !a.TryUpgrade() || !a.IsWrite() {
		t.Fatal("sole reader could not upgrade")
	}
	if l.RLockPath("/dir/file") != nil {
		t.Fatal("reader lock granted while upgraded to writer")
	}
	a.Unlock()

	b := l.RLockPath("/dir/file")
	c := l.RLockPath("/dir/file")
	if b.TryUpgrade() {
		t.Fatal("upgraded with a second reader on the path")
	}
	c.Unlock()
	child := l.RLockPath("/dir/file/child")
	if b.TryUpgrade() {
		t.Fatal("upgraded with a reader below the path")
	}
	child.Unlock()
	if !b.TryUpgrade() {
		t.Fatal("could not upgrade once the other readers left")
	}
	b.Unlock()
	if d := l.LockPath("/dir/file"); d == nil {
		t.Fatal("writer lock not released")
	} else {
		d.Unlock()
	}
	if r := l.RLockPath("/"); r.TryUpgrade() {
		t.Fatal("root upgraded to a writer lock")
	}
}

// Explorer opens files and folders with DELETE access while its own handles
// on them, or on files inside them, are open. Windows allows that when the
// other handles share delete; gofs used to refuse it with a sharing violation.
func TestOpenWithDeleteAccessWhileInUse(t *testing.T) {
	root, _ := mountFixture(t)
	file := filepath.Join(root, "hello.txt")
	dir := filepath.Join(root, "Dir")

	openShared(t, file, windows.GENERIC_READ)
	if err := tryOpen(file, windows.DELETE|windows.FILE_READ_ATTRIBUTES); err != nil {
		t.Errorf("DELETE open of a file another handle has open: %v", err)
	}

	openShared(t, filepath.Join(dir, "Sub", "File.txt"), windows.GENERIC_READ)
	if err := tryOpen(dir, windows.DELETE|windows.FILE_READ_ATTRIBUTES); err != nil {
		t.Errorf("DELETE open of a folder with a file open inside: %v", err)
	}
	if err := tryOpen(root, windows.DELETE|windows.FILE_READ_ATTRIBUTES); err != nil {
		t.Errorf("DELETE open of the root: %v", err)
	}
}

// Renaming still fails while something inside is open, as on NTFS, and
// works once it is closed.
func TestRenameWaitsForOpenHandles(t *testing.T) {
	root, _ := mountFixture(t)
	src := filepath.Join(root, "newdir")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(filepath.Join(src, "a.txt")), windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "renamed")
	if err := os.Rename(src, dst); err == nil {
		t.Error("renamed a folder with a file open inside")
	}
	windows.CloseHandle(h)
	if err := os.Rename(src, dst); err != nil {
		t.Errorf("rename after closing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Errorf("renamed folder content: %v", err)
	}
}

// A file can be replaced (the temp-file-and-swap Explorer and editors use)
// and deleted with the usual calls.
func TestReplaceAndDeleteFile(t *testing.T) {
	root, _ := mountFixture(t)
	target := filepath.Join(root, "hello.txt")
	tmp := filepath.Join(root, "hello.tmp")
	if err := os.WriteFile(tmp, []byte("replaced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := windows.MoveFileEx(windows.StringToUTF16Ptr(tmp), windows.StringToUTF16Ptr(target),
		windows.MOVEFILE_REPLACE_EXISTING); err != nil {
		t.Fatalf("MoveFileEx replace: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "replaced\n" {
		t.Errorf("after replace: %q", b)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleted file still visible: %v", err)
	}
}
