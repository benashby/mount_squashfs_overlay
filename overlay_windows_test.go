//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Without -overlay the upper dir is "". Joining "" with a rooted overlay path
// (e.g. `\Windows`) used to resolve against the root of the process's current
// drive, so that drive's contents appeared merged into the mount.
func TestReadOnlyMountDoesNotExposeCurrentDrive(t *testing.T) {
	sq := openFixture(t) // opened relative to the package dir, before Chdir
	// Make sure the current drive has a well-known root entry to leak.
	sysRoot := os.Getenv("SystemRoot") // e.g. C:\Windows
	if sysRoot == "" {
		t.Skip("SystemRoot not set")
	}
	t.Chdir(filepath.VolumeName(sysRoot) + `\`)
	leak := `\` + filepath.Base(sysRoot)

	ofs := &OverlayFileSystem{squash: sq} // read-only: upperDir == ""

	if _, err := ofs.Stat(leak); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%q) err = %v, want not-exist (host drive leaked into mount)", leak, err)
	}
	if f, err := ofs.OpenFile(leak, os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Errorf("OpenFile(%q) succeeded, want not-exist (host drive leaked into mount)", leak)
	}

	var got []string
	for _, fi := range ofs.mergeDir(`\`) {
		got = append(got, fi.Name())
	}
	sort.Strings(got)
	want := []string{"Dir", "big.bin", "hello.txt"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("root listing = %q, want %q", got, want)
	}

	// Files from the archive still resolve.
	if _, err := ofs.Stat(`\hello.txt`); err != nil {
		t.Errorf("Stat(hello.txt): %v", err)
	}
}

func TestReadOnlyMountRejectsCreate(t *testing.T) {
	sq := openFixture(t)
	t.Chdir(t.TempDir())
	ofs := &OverlayFileSystem{squash: sq}

	// O_CREATE without a write flag must not create anything either.
	name := `\squashoverlay-readonly-create-probe`
	if f, err := ofs.OpenFile(name, os.O_CREATE|os.O_RDONLY, 0o644); err == nil {
		f.Close()
		os.Remove(name)
		t.Fatalf("OpenFile(O_CREATE) succeeded in read-only mode")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("OpenFile(O_CREATE) err = %v, want permission denied", err)
	}
	if _, err := os.Stat(name); err == nil {
		os.Remove(name)
		t.Errorf("read-only OpenFile(O_CREATE) created %s on the host", name)
	}
	if err := ofs.Mkdir(`\newdir`, 0o755); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Mkdir err = %v, want permission denied", err)
	}
}

// With an overlay the upper layer must still shadow and extend the archive.
func TestOverlayMountUsesUpperDir(t *testing.T) {
	sq := openFixture(t)
	upper := t.TempDir()
	if err := os.WriteFile(filepath.Join(upper, "hello.txt"), []byte("shadowed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ofs := &OverlayFileSystem{squash: sq, upperDir: upper}

	fi, err := ofs.Stat(`\hello.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len("shadowed\n")) {
		t.Errorf("hello.txt size = %d, want the upper copy", fi.Size())
	}
	names := map[string]bool{}
	for _, fi := range ofs.mergeDir(`\`) {
		names[fi.Name()] = true
	}
	for _, n := range []string{"Dir", "big.bin", "hello.txt", "extra.txt"} {
		if !names[n] {
			t.Errorf("root listing missing %q (got %v)", n, names)
		}
	}
}
