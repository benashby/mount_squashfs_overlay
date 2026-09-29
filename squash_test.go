package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testdata/fixture.sqfs is generated from the tree described by fixtureFiles
// by TestGenerateFixture (see below). It uses a 1 MiB block size so that
// big.bin contains a data block stored uncompressed at exactly 1 MiB. Its
// on-disk length, 0x100000, does not fit in 20 bits.
const (
	fixturePath      = "testdata/fixture.sqfs"
	fixtureBlockSize = 1 << 20
)

var genFixture = flag.Bool("gen-fixture", false,
	"regenerate "+fixturePath+" with gensquashfs (squashfs-tools-ng) from $GENSQUASHFS or PATH")

// fixtureBig returns the deterministic content of big.bin:
//
//	block 0: 1 MiB of random bytes  -> incompressible, stored uncompressed
//	block 1: 1 MiB of text          -> compressed; its offset depends on block 0's size
//	tail   : 200 KiB of random bytes -> fragment
func fixtureBig() []byte {
	rng := rand.NewChaCha8([32]byte{'s', 'q', 'u', 'a', 's', 'h', 'o', 'v', 'e', 'r', 'l', 'a', 'y'})
	random := func(n int) []byte {
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	var text bytes.Buffer
	for i := 0; text.Len() < fixtureBlockSize; i++ {
		fmt.Fprintf(&text, "squashoverlay fixture line %d\n", i)
	}
	out := random(fixtureBlockSize)
	out = append(out, text.Bytes()[:fixtureBlockSize]...)
	out = append(out, random(200<<10)...)
	return out
}

// fixtureFiles maps fs.FS-style paths to their content.
func fixtureFiles() map[string][]byte {
	return map[string][]byte{
		"hello.txt":        []byte("hello from squashfs\n"),
		"Dir/Sub/File.txt": []byte("nested\n"),
		"big.bin":          fixtureBig(),
	}
}

func openFixture(t *testing.T) *SquashLayer {
	t.Helper()
	sq, err := NewSquashLayer(fixturePath)
	if err != nil {
		t.Fatalf("NewSquashLayer(%s): %v", fixturePath, err)
	}
	return sq
}

// TestGenerateFixture rebuilds testdata/fixture.sqfs. It is skipped unless
// -gen-fixture is given:
//
//	go test -run TestGenerateFixture -gen-fixture .
func TestGenerateFixture(t *testing.T) {
	if !*genFixture {
		t.Skip("pass -gen-fixture to regenerate " + fixturePath)
	}
	gen := os.Getenv("GENSQUASHFS")
	if gen == "" {
		gen = "gensquashfs"
	}
	src := t.TempDir()
	for name, data := range fixtureFiles() {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(gen,
		"--pack-dir", src,
		"--block-size", fmt.Sprint(fixtureBlockSize),
		"--compressor", "gzip", // the only codec KarpelesLab/squashfs supports without build tags
		"--all-root",
		"--num-jobs", "1",
		"--force",
		fixturePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", gen, err, out)
	}
}

// Check that the fixture contains what the tests below rely on.
func TestFixtureHasUncompressedFullBlock(t *testing.T) {
	sq := openFixture(t)
	if got := sq.sb.BlockSize; got != fixtureBlockSize {
		t.Fatalf("fixture block size = %d, want %d", got, fixtureBlockSize)
	}
	ino, err := sq.sb.FindInode("big.bin", true)
	if err != nil {
		t.Fatal(err)
	}
	const uncompressed = 1 << 24
	if b := ino.Blocks[0]; b&uncompressed == 0 || b&^uncompressed != fixtureBlockSize {
		t.Fatalf("big.bin block 0 = %#x, want an uncompressed %d-byte block", b, fixtureBlockSize)
	}
}

func TestSquashReadWholeFiles(t *testing.T) {
	sq := openFixture(t)
	for name, want := range fixtureFiles() {
		f, err := sq.Open("/" + name)
		if err != nil {
			t.Fatalf("Open(%s): %v", name, err)
		}
		got, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch (got %d bytes, want %d)", name, len(got), len(want))
		}
	}
}

// Reads that start inside, span, and follow a 1 MiB uncompressed block.
// Before the block-size mask fix these panicked (slice bounds out of range)
// or returned bytes from the wrong offset.
func TestSquashReadAtAcrossUncompressedFullBlock(t *testing.T) {
	for _, cache := range []int64{0, defaultCacheBytes} {
		sq := openFixture(t)
		sq.SetCacheSize(cache)
		t.Run(map[bool]string{true: "cached", false: "uncached"}[cache > 0], func(t *testing.T) {
			testReadAtAcrossUncompressedFullBlock(t, sq)
		})
	}
}

func testReadAtAcrossUncompressedFullBlock(t *testing.T, sq *SquashLayer) {
	want := fixtureBig()
	f, err := sq.Open("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ra := f.(io.ReaderAt)

	const B = fixtureBlockSize
	for _, c := range []struct{ off, n int }{
		{0, 4096},
		{B / 2, 4096},      // middle of the uncompressed block
		{B - 100, 200},     // spans uncompressed -> compressed block
		{B, 4096},          // start of the block after it
		{B + B/2, 4096},    // middle of the compressed block
		{2*B - 50, 100},    // spans compressed block -> fragment
		{2*B + 1000, 4096}, // inside the fragment
	} {
		buf := make([]byte, c.n)
		n, err := ra.ReadAt(buf, int64(c.off))
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(off=%d, n=%d): %v", c.off, c.n, err)
		}
		if !bytes.Equal(buf[:n], want[c.off:c.off+n]) || n != c.n {
			t.Errorf("ReadAt(off=%d, n=%d): got %d bytes, content mismatch=%v",
				c.off, c.n, n, !bytes.Equal(buf[:n], want[c.off:c.off+n]))
		}
	}
}

func TestSquashCaseInsensitiveOpen(t *testing.T) {
	sq := openFixture(t)
	info, err := sq.Stat("/dir/SUB/file.TXT")
	if err != nil {
		t.Fatalf("Stat with different casing: %v", err)
	}
	if info.Size() != int64(len("nested\n")) {
		t.Errorf("size = %d", info.Size())
	}
	if _, err := sq.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(/nope) err = %v, want not-exist", err)
	}
}
