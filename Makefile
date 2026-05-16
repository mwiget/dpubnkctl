.PHONY: build install test clean tidy fmt vet smoke slides slides-html slides-pptx slides-pdf

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BNK     := 2.2.0

LDFLAGS := -X 'github.com/mwiget/dpubnkctl/internal/version.Version=$(VERSION)' \
           -X 'github.com/mwiget/dpubnkctl/internal/version.Commit=$(COMMIT)' \
           -X 'github.com/mwiget/dpubnkctl/internal/version.BuildDate=$(DATE)' \
           -X 'github.com/mwiget/dpubnkctl/internal/version.BNKVersion=$(BNK)'

build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dpubnkctl ./cmd/dpubnkctl

install: build
	install -m 0755 bin/dpubnkctl $(HOME)/.local/bin/dpubnkctl

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w .

vet:
	go vet ./...

smoke: build
	@echo "--- version ---"
	./bin/dpubnkctl version
	@echo "--- init smoke ---"
	@rm -rf /tmp/dpubnkctl-smoke
	./bin/dpubnkctl init smoke --dir /tmp/dpubnkctl-smoke
	@ls /tmp/dpubnkctl-smoke
	@echo "--- agent claude ---"
	./bin/dpubnkctl agent claude --poc /tmp/dpubnkctl-smoke
	@echo "--- samples list ---"
	./bin/dpubnkctl samples
	@echo "--- init --sample two-node-homelab ---"
	@rm -rf /tmp/dpubnkctl-smoke-sample
	./bin/dpubnkctl init smoke-sample --sample two-node-homelab \
	    --dir /tmp/dpubnkctl-smoke-sample --no-git
	@grep -q "name: smoke-sample" /tmp/dpubnkctl-smoke-sample/poc.yaml \
	    || (echo "ERR: --sample did not patch metadata.name" && exit 1)
	@grep -q "CUSTOMIZE" /tmp/dpubnkctl-smoke-sample/poc.yaml \
	    || (echo "ERR: CUSTOMIZE markers stripped" && exit 1)

clean:
	rm -rf bin/

# --- slide deck ---------------------------------------------------------
#
# Source: docs/slides/*.md (Marp markdown — readable on GitHub as-is).
# `make slides` produces HTML (no extra tooling), PPTX, and PDF in one go.
# PPTX + PDF need a Chromium/Firefox binary at $CHROME_PATH (or installed
# on PATH); HTML is the browser-free fallback. Marp-cli is fetched on
# demand via `npx`, no global install needed.

SLIDE_SRC := $(wildcard docs/slides/*.md)
SLIDE_HTML := $(SLIDE_SRC:.md=.html)
SLIDE_PPTX := $(SLIDE_SRC:.md=.pptx)
SLIDE_PDF  := $(SLIDE_SRC:.md=.pdf)

slides: slides-html slides-pptx slides-pdf

slides-html: $(SLIDE_HTML)

slides-pptx: $(SLIDE_PPTX)

slides-pdf: $(SLIDE_PDF)

docs/slides/%.html: docs/slides/%.md
	npx --yes @marp-team/marp-cli $< -o $@

docs/slides/%.pptx: docs/slides/%.md
	npx --yes @marp-team/marp-cli $< -o $@ --allow-local-files

docs/slides/%.pdf: docs/slides/%.md
	npx --yes @marp-team/marp-cli $< -o $@ --allow-local-files
