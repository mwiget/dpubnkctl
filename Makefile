.PHONY: all build build-linux-arm64 build-all install test clean tidy fmt vet smoke slides

# Default target: build both binaries. The host-native build is what
# the developer runs (typically darwin/arm64 on a Mac); the linux/arm64
# build is what a Claude Code agent inside the Docker Desktop sandbox
# can execute. Same source, same ldflags.
all: build-all

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

# Cross-build for the Linux/arm64 sandbox a Claude Code agent runs in
# when the developer host is an Apple Silicon Mac. Same source, same
# ldflags — just a different GOOS so the agent can `validate` / `init`
# / `samples` against the working tree without a darwin Mach-O blocker.
build-linux-arm64:
	@mkdir -p bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o bin/dpubnkctl-linux-arm64 ./cmd/dpubnkctl

# Build both binaries — host-native (typically darwin/arm64 on a Mac)
# and linux/arm64 for the agent sandbox. Run this after any change
# that an agent might want to validate end-to-end.
build-all: build build-linux-arm64

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
# `make slides` renders HTML via npx + marp-cli; no extra tooling.
#
# PPTX/PDF intentionally dropped — Marp's PPTX export is a slide-per-
# image bundle that can't be edited, and the PDF is just a worse-
# fidelity copy of the HTML the slides folder already ships. If you
# need either, render locally with `npx @marp-team/marp-cli --pptx`
# or `--pdf`; we don't track them in git.

SLIDE_SRC := $(filter-out docs/slides/README.md,$(wildcard docs/slides/*.md))
SLIDE_HTML := $(SLIDE_SRC:.md=.html)

slides: $(SLIDE_HTML)

docs/slides/%.html: docs/slides/%.md
	npx --yes @marp-team/marp-cli $< -o $@
