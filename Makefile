# squashoverlay – Makefile
#
# Build a Windows .exe from Linux using go-winfsp.
# No CGO or mingw-w64 is required.
#
# Build targets:
#   make              – build mount.exe
#   make vendor       – go mod vendor + apply patches
#   make clean        – remove built binary
#   make test         – run unit tests on the host OS

BINARY      := mount.exe
GOOS        := windows
GOARCH      := amd64

.PHONY: all vendor clean help winres test

all: $(BINARY)

# ── Windows resources (manifest + version info) ───────────────────────────
winres:
	@echo "Generating Windows resources..."
	go-winres make
	@echo "Done: rsrc_windows_*.syso"

# ── Vendor: fetch upstream modules then apply local patches ───────────────
vendor:
	@echo "Running go mod vendor..."
	go mod vendor
	@echo "Applying patches..."
	@for p in patches/*.patch; do \
		echo "  $$p"; \
		patch -p1 --no-backup-if-mismatch < "$$p"; \
	done
	@echo "Done: vendor/ is ready"

# ── Binary ─────────────────────────────────────────────────────────────────
# vendor/ sentinel: re-vendor automatically if the directory is absent.
vendor/modules.txt:
	$(MAKE) vendor

$(BINARY): vendor/modules.txt $(shell find . -path ./vendor -prune -o -name '*.go' -print) winres
	@echo "Building $(BINARY) (GOOS=$(GOOS) GOARCH=$(GOARCH))..."
	CGO_ENABLED=0 \
	GOOS=$(GOOS) \
	GOARCH=$(GOARCH) \
	go build -tags "xz zstd" -ldflags="-s -w" -trimpath -o $(BINARY) .
	@echo "Done: $(BINARY)"

# ── Tests ────────────────────────────────────────────────────────────────
# Root package only: tests/ holds standalone programs, each with its own main().
test:
	go test -tags "xz zstd" .

# ── Housekeeping ───────────────────────────────────────────────────────────
clean:
	rm -f $(BINARY)

# ── Quick help ─────────────────────────────────────────────────────────────
help:
	@echo "Targets:"
	@echo "  make          build mount.exe (vendors automatically if needed)"
	@echo "  make vendor   go mod vendor + apply patches/"
	@echo "  make clean    remove binary"
	@echo "  make test     run unit tests"
