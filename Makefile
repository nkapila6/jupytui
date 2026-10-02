VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64
TAPES   := $(wildcard demo/*.tape)

export CGO_ENABLED=0

.DEFAULT_GOAL := build
.PHONY: help build install test lint dist demos demo-kitty clean

help:
	@echo "make build       static, stripped ./jupytui"
	@echo "make install     go install with the version baked in"
	@echo "make test        go vet + go test"
	@echo "make lint        staticcheck"
	@echo "make dist        release tarballs + checksums in dist/"
	@echo "make demos       re-record the VHS GIFs in demo/ (needs vhs, uv)"
	@echo "make demo-kitty  play the tour in this terminal, for the kitty recording"
	@echo "make clean       remove build output"

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o jupytui ./cmd/jupytui

install:
	go install -trimpath -ldflags="$(LDFLAGS)" ./cmd/jupytui

test:
	go vet ./...
	go test ./...

lint:
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

# one tarball per target plus checksums, for GitHub releases
dist: clean
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; name=jupytui_$(VERSION)_$${os}_$${arch}; \
		echo "building $$name"; \
		mkdir -p dist/$$name && \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$name/jupytui ./cmd/jupytui && \
		cp README.md dist/$$name/ && \
		tar -C dist -czf dist/$$name.tar.gz $$name && rm -r dist/$$name; \
	done
	@cd dist && shasum -a 256 *.tar.gz > checksums.txt

demos: build
	@for t in $(TAPES); do echo "recording $$t"; vhs $$t || exit 1; done

# the top README GIF: run this inside kitty/Ghostty while screen recording.
# /usr/bin/python3 because some homebrew pythons leak PYTHONPATH to children
demo-kitty: build
	/usr/bin/python3 demo/play.py

clean:
	rm -rf dist jupytui demo/scratch*
