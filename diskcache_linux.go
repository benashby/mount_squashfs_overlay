//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// Sparse by default on Linux filesystems.
func setSparse(*os.File) error { return nil }

// punchHole releases the disk space of [off, off+n).
func punchHole(f *os.File, off, n int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
}

// lockFile takes an exclusive lock on f without waiting.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func diskFreeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
