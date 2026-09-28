package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const version = "1.1.0"

// CLI interface is designed as a drop-in replacement for the mount.exe used by
// EmulatorLauncher (github.com/RetroBat-Official/emulatorlauncher).
//
// Invocation by the launcher:
//
//	mount.exe [-debug] -drive Z: [-extractionpath "<path>"] [-overlay "<path>"] "<squashfs-file>"
//
// The process runs until killed; killing it unmounts the drive.
func main() {
	debug := flag.Bool("debug", false, "enable verbose debug output")
	logFile := flag.String("log", "", "write debug output to this file (implies -debug)")
	drive := flag.String("drive", "", "drive letter to mount at, e.g. Z:")
	flag.String("extractionpath", "", "accepted for compatibility; ignored")
	overlayPath := flag.String("overlay", "", "persistent writable overlay directory")
	cacheMB := flag.Int64("cache-mb", defaultCacheBytes>>20, "decompressed block cache size in MiB (0 disables)")
	diskCacheDir := flag.String("disk-cache", "", "keep a local copy of the image's data in this directory")
	diskCacheGB := flag.Int64("disk-cache-gb", 200, "size cap for -disk-cache, in GiB (0 = no cap)")
	diskCacheFreeGB := flag.Int64("disk-cache-min-free-gb", 150, "free space -disk-cache leaves on its disk, in GiB (0 = no floor)")
	prefetch := flag.Bool("prefetch", false, "with -disk-cache, copy the whole image in the background while mounted")
	flag.Usage = usage
	flag.Parse()

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fatalf("cannot open log file %q: %v", *logFile, err)
		}
		os.Stderr = f
		*debug = true
	}

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "error: squashfs file argument is required\n\n")
		usage()
		os.Exit(1)
	}
	if *drive == "" {
		fmt.Fprintf(os.Stderr, "error: -drive is required\n\n")
		usage()
		os.Exit(1)
	}

	squashFile, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fatalf("invalid squashfs path: %v", err)
	}
	if *diskCacheDir == "" {
		// With -disk-cache an unreachable image can still mount from the cache.
		if _, err := os.Stat(squashFile); err != nil {
			fatalf("cannot open squashfs file %q: %v", squashFile, err)
		}
	}

	driveLetter := normalizeDrive(*drive)

	// -overlay is the writable upper directory; -extractionpath is accepted
	// for compatibility with existing EmulatorLauncher invocations but ignored.
	// If neither is given the drive is mounted read-only.
	var upperDir string
	if *overlayPath != "" {
		upperDir = *overlayPath
		if err := os.MkdirAll(upperDir, 0755); err != nil {
			fatalf("cannot create overlay dir %q: %v", upperDir, err)
		}
		if err := migrateDeletions(upperDir); err != nil {
			fatalf("failed to migrate .deletions: %v", err)
		}
	}

	if *debug {
		fmt.Fprintf(os.Stderr, "squashoverlay v%s\n", version)
		fmt.Fprintf(os.Stderr, "  squashfs    : %s\n", squashFile)
		fmt.Fprintf(os.Stderr, "  drive       : %s\n", driveLetter)
		if upperDir != "" {
			fmt.Fprintf(os.Stderr, "  upper dir   : %s\n", upperDir)
		} else {
			fmt.Fprintf(os.Stderr, "  upper dir   : (none — read-only)\n")
		}
		fmt.Fprintf(os.Stderr, "  overlay arg : %s\n", *overlayPath)
	}

	if err := checkWinFsp(); err != nil {
		fatalf("WinFsp not available: %v\n\nDownload and install WinFsp from:\nhttps://github.com/winfsp/winfsp/releases", err)
	}

	var sq *SquashLayer
	var img *CachedImage
	if *diskCacheDir != "" {
		dc, err := NewDiskCache(*diskCacheDir, *diskCacheGB<<30, *diskCacheFreeGB<<30)
		if err != nil {
			fatalf("cannot open disk cache %q: %v", *diskCacheDir, err)
		}
		sq, img, err = NewCachedSquashLayer(squashFile, dc)
		if err != nil {
			fatalf("failed to open squashfs %q: %v", squashFile, err)
		}
		switch {
		case img == nil:
			fmt.Fprintln(os.Stderr, "disk cache for this image is in use by another mount; reading it directly")
		case !img.Online():
			fmt.Fprintln(os.Stderr, "image unreachable; serving from the disk cache only")
		case *prefetch:
			img.StartPrefetch(func(n int, err error) {
				if *debug || err != nil {
					fmt.Fprintf(os.Stderr, "prefetch stopped after %d chunks: %v\n", n, err)
				}
			})
		}
	} else {
		var err error
		if sq, err = NewSquashLayer(squashFile); err != nil {
			fatalf("failed to open squashfs %q: %v", squashFile, err)
		}
	}
	sq.SetCacheSize(*cacheMB << 20)

	if *debug {
		fmt.Fprintf(os.Stderr, "Mounting %s at %s ...\n", squashFile, driveLetter)
	}

	// Mount blocks until the filesystem is unmounted (i.e. this process is killed).
	err = Mount(sq, upperDir, driveLetter, *debug)
	if img != nil {
		img.Close()
	}
	if err != nil {
		fatalf("mount failed: %v", err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `squashoverlay v%s - mount a squashfs file as a Windows drive with persistent writable overlay

Usage:
  squashoverlay.exe [-debug] [-log <file>] -drive <X:> [-extractionpath <dir>] [-overlay <dir>] <squashfs-file>

Flags:
  -drive <X:>            Drive letter to mount at (required)
  -extractionpath <dir>  Work/extraction directory (used as overlay if -overlay not given)
  -overlay <dir>         Persistent writable overlay directory (takes precedence)
  -cache-mb <n>          Decompressed block cache size in MiB (default %d; 0 disables)
  -disk-cache <dir>      Keep a local copy of the image's data in <dir>; least
                         recently used data is released automatically
  -disk-cache-gb <n>     Size cap for -disk-cache in GiB (default 200; 0 = none)
  -disk-cache-min-free-gb <n>
                         Free space -disk-cache leaves on its disk in GiB
                         (default 150; 0 = none)
  -prefetch              With -disk-cache, copy the whole image in the
                         background while it is mounted
  -debug                 Verbose output to stderr
  -log <file>            Write verbose output to <file> (implies -debug)

The process runs until killed; killing it unmounts the drive.
Requires WinFsp >= 1.10: https://github.com/winfsp/winfsp/releases
`, version, defaultCacheBytes>>20)
}

func normalizeDrive(s string) string {
	s = strings.TrimSuffix(s, `\`)
	s = strings.TrimSuffix(s, "/")
	s = strings.ToUpper(s)
	if !strings.HasSuffix(s, ":") {
		s += ":"
	}
	return s
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
