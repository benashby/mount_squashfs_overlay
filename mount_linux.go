//go:build !windows

package main

import (
	"fmt"
	"os/exec"

	"github.com/winfsp/cgofuse/fuse"
)

// Mount mounts the overlay filesystem at mountpoint using libfuse (Linux/macOS).
// On Linux this is useful for development and testing without a Windows machine.
// mountpoint should be an existing directory (e.g. "/mnt/test").
func Mount(sq *SquashLayer, upperDir string, mountpoint string, opts MountOptions) error {
	overlayFS := &OverlayFS{
		squash:   sq,
		upperDir: upperDir,
		debug:    opts.Debug,
	}

	host := fuse.NewFileSystemHost(overlayFS)
	host.SetCapReaddirPlus(true)

	ok := host.Mount(mountpoint, []string{"-o", "default_permissions"})
	if !ok {
		return fmt.Errorf("fuse mount failed for %s", mountpoint)
	}
	return nil
}

// Umount unmounts a libfuse mountpoint (Linux dev/test use only).
func Umount(mountpoint string) error {
	cmd := exec.Command("fusermount", "-u", mountpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fusermount -u %s: %v\n%s", mountpoint, err, out)
	}
	return nil
}
