# squashoverlay

squashoverlay is a drop-in replacement for the `mount.exe` used by
[EmulatorLauncher](https://github.com/RetroBat-Official/emulatorlauncher).
It mounts a read-only squashfs archive as a Windows drive letter,
with a persistent writable overlay directory layered on top using
Docker/OCI-style whiteout conventions (`.wh.<name>` / `.wh..wh..opq`).
Renaming a folder that comes from the archive copies nothing: the new
folder gets a `.wh..wh..redirect` file holding the archive path of its
contents, and the old name gets a whiteout.

## Architecture

- **Lower layer (read-only):** squashfs archive, opened via two libraries:
  - `KarpelesLab/squashfs` — FUSE reads (Open/Stat/ReadDir/Readlink), per-block decompression with ~128 KB RAM per open file.
  - `CalebQ42/squashfs` — copy-on-write materialisation, parallel WriteTo with sync.Pool for efficient bulk decompression.
- **Upper layer (writable):** host directory; receives CoW copies on first write and stores whiteout markers for deleted entries.
- **Case-insensitive path resolution:** squashfs names are matched case-insensitively via a lazily-built, immutably-cached dirIndex.

On Windows the VFS is served through [WinFsp](https://github.com/winfsp/winfsp) (go-winfsp, pure-Go, no CGO).
On Linux a cgofuse/libfuse bridge is used for testing.

## Usage

```
mount.exe [-debug] -drive <X:> [-extractionpath <dir>] [-overlay <dir>] [-cache-mb <n>]
          [-disk-cache <dir> [-disk-cache-gb <n>] [-disk-cache-min-free-gb <n>] [-prefetch]]
          <squashfs-file>
```

| Flag | Description |
|------|-------------|
| `-drive <X:>` | Drive letter to mount at (required) |
| `-overlay <dir>` | Persistent writable overlay directory (omit for read-only mount) |
| `-extractionpath <dir>` | Accepted for compatibility with EmulatorLauncher; ignored |
| `-cache-mb <n>` | Decompressed block cache in MiB, shared by all open files (default 256; `0` disables) |
| `-disk-cache <dir>` | Keep a local copy of the image's data in `<dir>` (see below) |
| `-disk-cache-gb <n>` | Size cap for the disk cache in GiB (default 200; `0` = no cap) |
| `-disk-cache-min-free-gb <n>` | Free space the disk cache leaves on its disk in GiB (default 150; `0` = no floor) |
| `-prefetch` | With `-disk-cache`, copy the whole image in the background while it is mounted |
| `-debug` | Verbose output |

The process runs until killed; killing it unmounts the drive.

### Example

```
mount.exe -drive Z: -extractionpath "C:\Temp\work" -overlay "C:\saves\game1" "C:\roms\game.squashfs"
```

This mounts `game.squashfs` at `Z:`, with any writes or deletions persisted into
`C:\saves\game1\` so they survive remount.

If a RetroBat-style `.deletions` text file is found in the overlay directory it is
automatically converted to whiteout files on first mount and then removed.

### Disk cache

For images on a network share, `-disk-cache <dir>` keeps a local copy of
every part of the image that has been read. Later reads of those parts,
including after a remount or reboot, come from local disk. The copy is a
sparse file per image, so it only takes space for what was read, and it holds
the image's compressed bytes. If the share is unreachable, an image mounts
from the cache alone, and reads of parts that were never cached fail.

The cache looks after itself. When it grows past `-disk-cache-gb`, or the disk
it lives on drops below `-disk-cache-min-free-gb` of free space, the least
recently used 4 MiB chunks are released, across all images in the directory.
A cache is tied to the image's size, modification time and superblock, so
replacing an image discards its old cache. Each chunk is checksummed and
fetched again if the local copy is damaged.

Several mounts can share one cache directory. If two processes mount the
same image, the second reads it directly without the cache.

## Comparison with the original EmulatorLauncher mount.exe

The original uses Dokan 2 and extracts files to a temp directory on first access via an external `rdsquashfs.exe` subprocess. squashoverlay streams directly from the squashfs archive in-process.

| Aspect | Original (Dokan + rdsquashfs) | squashoverlay (WinFsp) |
|--------|-------------------------------|------------------------|
| **Filesystem driver** | Dokan 2 | WinFsp |
| **File access** | Extract to disk on first access, read from disk cache after | Stream from squashfs, decompress per-block on demand |
| **Temp disk space** | Required (full file extraction) | None |
| **Startup time** | O(n files) — full archive listing pre-loaded | Near-instant — metadata read lazily |
| **First file access** | Slow — spawns `rdsquashfs.exe` subprocess | Fast — decompresses only requested ~128 KB block |
| **Repeated access** | Fast — plain disk I/O from OS page cache | Fast — squashfs blocks cached by OS page cache |
| **Decompression** | External process (`rdsquashfs.exe`) | In-process, no subprocess overhead |
| **Concurrent reads** | Lock per file | Lock-free via `io.ReaderAt` on shared file handle |
| **CoW copy** | Via `rdsquashfs.exe` subprocess | In-process, parallel `WriteTo` with `sync.Pool` |
| **Overlay format** | Custom `.deletions` text file | Docker/OCI whiteout files (`.wh.*`) |

For the typical emulator workload (mount → launch game → read files once → quit) squashoverlay is faster: no extraction wait, no temp disk usage, and lower first-access latency. The original's disk cache advantage only applies to workloads that read the same files heavily within one session.

## Requirements

- [WinFsp](https://github.com/winfsp/winfsp/releases) >= 1.10

## Build

```
make
```

Cross-compiles from Linux to Windows with `CGO_ENABLED=0 GOOS=windows GOARCH=amd64`.
Build tags `xz` and `zstd` are enabled automatically (see [Makefile](Makefile)).

## Test

```
make test        # or: go test -tags "xz zstd" .
```

All tests run on Windows; on Linux only the squashfs-layer tests apply (the
overlay tests are Windows-only, and building needs libfuse for cgofuse).
`go test ./...` fails because `tests/` holds standalone programs, each with
its own `main()`, so test the root package only.

`testdata/fixture.sqfs` is a small gzip image with a 1 MiB block size. Its
content is generated deterministically by `squash_test.go`; to rebuild it you
need `gensquashfs` from [squashfs-tools-ng](https://github.com/AgentD/squashfs-tools-ng)
on `PATH` (or in `$GENSQUASHFS`):

```
go test -run TestGenerateFixture -gen-fixture .
```
