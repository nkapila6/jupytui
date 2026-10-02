VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64

export CGO_ENABLED=0

.PHONY: build install test dist clean demos

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o jupytui ./cmd/jupytui

install:
	go install -trimpath -ldflags="$(LDFLAGS)" ./cmd/jupytui

test:
	go vet ./...
	go test ./...

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

# re-record the README GIFs (needs vhs and uv)
demos:
	for t in demo/*.tape; do vhs $$t; done

clean:
	rm -rf dist jupytui
