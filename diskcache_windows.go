//go:build windows

package main

import (
	"encoding/binary"
	"os"

	"golang.org/x/sys/windows"
)

const (
	fsctlSetSparse   = 0x000900c4
	fsctlSetZeroData = 0x000980c8
)

func setSparse(f *os.File) error {
	var n uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &n, nil)
}

// punchHole releases the disk space of [off, off+n) in a sparse file.
func punchHole(f *os.File, off, n int64) error {
	var in [16]byte // FILE_ZERO_DATA_INFORMATION
	binary.LittleEndian.PutUint64(in[:], uint64(off))
	binary.LittleEndian.PutUint64(in[8:], uint64(off+n))
	var ret uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetZeroData,
		&in[0], uint32(len(in)), nil, 0, &ret, nil)
}

// lockFile takes an exclusive lock on f without waiting.
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

func diskFreeSpace(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return int64(avail), nil
}
