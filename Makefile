.PHONY: build install test clean tidy fmt vet smoke

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

clean:
	rm -rf bin/
