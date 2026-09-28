//go:build windows

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func newOverlay(t *testing.T, upper string) *OverlayFileSystem {
	t.Helper()
	return &OverlayFileSystem{squash: openFixture(t), upperDir: upper}
}

func names(ofs *OverlayFileSystem, dir string) []string {
	var n []string
	for _, fi := range ofs.mergeDir(dir) {
		n = append(n, fi.Name())
	}
	sort.Strings(n)
	return n
}

func readOverlay(t *testing.T, ofs *OverlayFileSystem, name string) string {
	t.Helper()
	f, err := ofs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, ofs *OverlayFileSystem, name string) {
	t.Helper()
	if _, err := ofs.Stat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%s) err = %v, want not-exist", name, err)
	}
}

// Renaming a directory that only exists in the image moves its contents
// under the new name without copying them.
func TestRenameImageDirectory(t *testing.T) {
	upper := t.TempDir()
	ofs := newOverlay(t, upper)

	if err := ofs.Rename(`\Dir`, `\Renamed`); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	mustNotExist(t, ofs, `\Dir`)
	mustNotExist(t, ofs, `\Dir\Sub\File.txt`)
	if got := readOverlay(t, ofs, `\Renamed\Sub\File.txt`); got != "nested\n" {
		t.Errorf("renamed content = %q", got)
	}
	if got := readOverlay(t, ofs, `\renamed\SUB\file.TXT`); got != "nested\n" {
		t.Errorf("case-insensitive lookup = %q", got)
	}
	if got, want := names(ofs, `\`), []string{"Renamed", "big.bin", "hello.txt"}; !equal(got, want) {
		t.Errorf("root = %q, want %q", got, want)
	}
	if got := names(ofs, `\Renamed`); !equal(got, []string{"Sub"}) {
		t.Errorf("renamed dir = %q", got)
	}
	// Nothing was copied: the upper dir only holds the marker.
	var files []string
	filepath.WalkDir(upper, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(upper, p)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	if want := []string{".wh.Dir", `Renamed\.wh..wh..redirect`}; !equal(files, want) {
		t.Errorf("upper files = %q, want %q", files, want)
	}
}

func TestRenamedDirectoryIsWritable(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	if err := ofs.Rename(`\Dir`, `\Renamed`); err != nil {
		t.Fatal(err)
	}
	f, err := ofs.OpenFile(`\Renamed\Sub\File.txt`, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatalf("open for write (copy-up): %v", err)
	}
	f.Write([]byte("changed\n"))
	f.Close()
	if got := readOverlay(t, ofs, `\Renamed\Sub\File.txt`); got != "changed\n" {
		t.Errorf("after write = %q", got)
	}
	f, err = ofs.OpenFile(`\Renamed\new.txt`, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := names(ofs, `\Renamed`); !equal(got, []string{"Sub", "new.txt"}) {
		t.Errorf("renamed dir = %q", got)
	}
}

func TestRenameChainsAndNesting(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	steps := [][2]string{
		{`\Dir`, `\A`},
		{`\A`, `\B`},               // renamed again
		{`\B\Sub`, `\B\Sub2`},      // directory inside a renamed one
		{`\B`, `\Nested\Deeper\C`}, // moved under new parents
	}
	for _, s := range steps {
		if err := ofs.Rename(s[0], s[1]); err != nil {
			t.Fatalf("Rename(%s, %s): %v", s[0], s[1], err)
		}
	}
	if got := readOverlay(t, ofs, `\Nested\Deeper\C\Sub2\File.txt`); got != "nested\n" {
		t.Errorf("content after chain = %q", got)
	}
	for _, gone := range []string{`\Dir`, `\A`, `\B`, `\Nested\Deeper\C\Sub`} {
		mustNotExist(t, ofs, gone)
	}
	// A new mount reads the markers back from the upper dir.
	again := newOverlay(t, ofs.upperDir)
	if got := readOverlay(t, again, `\Nested\Deeper\C\Sub2\File.txt`); got != "nested\n" {
		t.Errorf("after remount = %q", got)
	}
	mustNotExist(t, again, `\Dir`)
}

// A directory with contents in both layers keeps both after a rename.
func TestRenameMergedDirectory(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	f, err := ofs.OpenFile(`\Dir\added.txt`, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := ofs.Rename(`\Dir`, `\Merged`); err != nil {
		t.Fatal(err)
	}
	if got := names(ofs, `\Merged`); !equal(got, []string{"Sub", "added.txt"}) {
		t.Errorf("merged dir after rename = %q", got)
	}
	mustNotExist(t, ofs, `\Dir`)
}

func TestRemoveRenamedDirectory(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	if err := ofs.Rename(`\Dir`, `\Renamed`); err != nil {
		t.Fatal(err)
	}
	if err := ofs.Remove(`\Renamed`); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, ofs, `\Renamed`)
	mustNotExist(t, ofs, `\Renamed\Sub\File.txt`)
	mustNotExist(t, ofs, `\Dir`)
	if got, want := names(ofs, `\`), []string{"big.bin", "hello.txt"}; !equal(got, want) {
		t.Errorf("root = %q, want %q", got, want)
	}
}

// Through Windows, as Explorer does it.
func TestRenameImageDirectoryOnMountedDrive(t *testing.T) {
	root, _ := mountFixture(t)
	if err := os.Rename(filepath.Join(root, "Dir"), filepath.Join(root, "Renamed")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "Renamed", "Sub", "File.txt"))
	if err != nil || string(b) != "nested\n" {
		t.Errorf("read renamed file: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Dir")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old name still visible: %v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Files under a deleted directory stay hidden when opened by full path,
// as a game would, not only in listings.
func TestDeletedDirectoryHidesItsContents(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	if err := ofs.Remove(`\Dir`); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, ofs, `\Dir`)
	mustNotExist(t, ofs, `\Dir\Sub`)
	mustNotExist(t, ofs, `\Dir\Sub\File.txt`)
	if f, err := ofs.OpenFile(`\Dir\Sub\File.txt`, os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Error("opened a file inside a deleted directory")
	}
	if got := names(ofs, `\Dir\Sub`); len(got) != 0 {
		t.Errorf("listing of a deleted directory = %q", got)
	}
}

// A directory created where a deleted one was starts empty.
func TestRecreatedDirectoryIsEmpty(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	if err := ofs.Remove(`\Dir`); err != nil {
		t.Fatal(err)
	}
	if err := ofs.Mkdir(`\Dir`, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := names(ofs, `\Dir`); len(got) != 0 {
		t.Errorf("recreated dir = %q, want empty", got)
	}
	mustNotExist(t, ofs, `\Dir\Sub\File.txt`)
}

// Renaming a new directory onto a deleted name does not merge in the old
// contents.
func TestRenameOntoDeletedName(t *testing.T) {
	ofs := newOverlay(t, t.TempDir())
	if err := ofs.Remove(`\Dir`); err != nil {
		t.Fatal(err)
	}
	if err := ofs.Mkdir(`\fresh`, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ofs.Rename(`\fresh`, `\Dir`); err != nil {
		t.Fatal(err)
	}
	if got := names(ofs, `\Dir`); len(got) != 0 {
		t.Errorf("dir after rename onto deleted name = %q, want empty", got)
	}
	mustNotExist(t, ofs, `\Dir\Sub\File.txt`)
}
