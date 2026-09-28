//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

var procGetCompressedFileSizeW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// allocatedSize returns the disk space a (sparse) file uses.
func allocatedSize(t *testing.T, path string) int64 {
	t.Helper()
	p, _ := syscall.UTF16PtrFromString(path)
	var hi uint32
	lo, _, err := procGetCompressedFileSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&hi)))
	if uint32(lo) == 0xffffffff && err != syscall.Errno(0) {
		t.Fatalf("GetCompressedFileSizeW: %v", err)
	}
	return int64(hi)<<32 | int64(uint32(lo))
}

func TestPunchHoleReleasesSpace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sparse.data")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := setSparse(f); err != nil {
		t.Fatalf("setSparse: %v", err)
	}
	const size = 64 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if got := allocatedSize(t, p); got > 1<<20 {
		t.Fatalf("new sparse file allocates %d bytes", got)
	}
	chunk := make([]byte, 8<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	f.WriteAt(chunk, 16<<20)
	f.Sync()
	if got := allocatedSize(t, p); got < 8<<20 {
		t.Fatalf("after writing 8 MiB, allocated %d bytes", got)
	}
	if err := punchHole(f, 16<<20, 8<<20); err != nil {
		t.Fatalf("punchHole: %v", err)
	}
	f.Sync()
	if got := allocatedSize(t, p); got > 1<<20 {
		t.Errorf("after punching the hole, allocated %d bytes", got)
	}
	buf := make([]byte, 4096)
	f.ReadAt(buf, 20<<20)
	for _, b := range buf {
		if b != 0 {
			t.Fatal("punched range does not read back as zeros")
		}
	}
}
