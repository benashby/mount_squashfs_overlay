//go:build windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/gofs"
	"golang.org/x/sys/windows"
)

// fsName is reported as the volume's file system type. The Explorer
// integration uses it to recognise squashoverlay drives.
const fsName = "squashoverlay"

// Mount mounts the overlay filesystem at the given Windows drive letter (e.g. "Z:").
// This call blocks until the process is interrupted, the drive's unmount
// event is signalled (see unmountEventName), or the process is killed.
func Mount(sq *SquashLayer, upperDir string, drive string, opts MountOptions) error {
	overlayFS := &OverlayFileSystem{
		squash:   sq,
		upperDir: upperDir,
		debug:    opts.Debug,
	}

	if opts.Debug {
		fmt.Fprintf(os.Stderr, "Mounting with go-winfsp at %s ...\n", drive)
	}

	fs := gofs.New(overlayFS)
	if opts.Label != "" {
		if l, ok := fs.(winfsp.BehaviourSetVolumeLabel); ok {
			label := []rune(opts.Label)
			if len(label) > 31 { // 32 UTF-16 units including the terminator
				label = label[:31]
			}
			l.SetVolumeLabel(nil, string(label), &winfsp.FSP_FSCTL_VOLUME_INFO{})
		}
	}

	// Created before mounting so an unmount request can't be missed.
	ev, err := windows.CreateEvent(nil, 1, 0, windows.StringToUTF16Ptr(unmountEventName(drive)))
	if err != nil {
		return fmt.Errorf("creating unmount event: %w", err)
	}
	defer windows.CloseHandle(ev)

	ptfs, err := winfsp.Mount(fs, drive, winfsp.FileSystemName(fsName))
	if err != nil {
		return fmt.Errorf("WinFsp mount failed for %s: %w", drive, err)
	}
	defer ptfs.Unmount()

	state := mountStatePath(drive)
	if err := writeMountState(state, opts.Image, upperDir == ""); err != nil && opts.Debug {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", state, err)
	}
	defer os.Remove(state)

	if opts.Debug {
		fmt.Fprintf(os.Stderr, "Mounted successfully. Waiting for interrupt...\n")
	}

	// Block until the process is interrupted or the unmount event is set.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		windows.WaitForSingleObject(ev, windows.INFINITE)
		close(done)
	}()
	select {
	case <-ch:
	case <-done:
	}

	if opts.Debug {
		fmt.Fprintf(os.Stderr, "Unmounting...\n")
	}
	return nil
}

// Umount is a no-op on the go-winfsp path; the mount is cleaned up
// when the process exits. Provided for CLI interface compatibility.
func Umount(drive string) error {
	return nil
}

// unmountEventName is the session-local event that asks the mount of drive
// ("R:") to unmount cleanly.
func unmountEventName(drive string) string {
	return `Local\squashoverlay-unmount-` + strings.TrimSuffix(strings.ToUpper(drive), ":")
}

// stateDir holds per-user runtime files: mounts\, logs\ and cache\.
func stateDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "squashoverlay")
	}
	return filepath.Join(os.TempDir(), "squashoverlay")
}

// mountStatePath is the file describing the mount on drive, read by the
// Explorer integration.
func mountStatePath(drive string) string {
	return filepath.Join(stateDir(), "mounts", strings.TrimSuffix(strings.ToUpper(drive), ":")+".txt")
}

// writeMountState records the mount as "key=value" lines.
func writeMountState(path, image string, readOnly bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	ro := "0"
	if readOnly {
		ro = "1"
	}
	body := fmt.Sprintf("image=%s\r\npid=%d\r\nreadonly=%s\r\n", image, os.Getpid(), ro)
	return os.WriteFile(path, []byte(body), 0o644)
}
