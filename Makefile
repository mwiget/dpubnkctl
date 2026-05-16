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
# `make slides` produces HTML, PPTX, and PDF in one go.
#
# HTML uses npx + marp-cli; no extra tooling.
# PPTX/PDF need Chromium/Firefox — either locally at $CHROME_PATH, or
# the docker fallback used by these targets when no local browser is
# detected. The docker fallback runs marpteam/marp-cli inside its own
# container (Chromium baked in), bind-mounts docs/slides, and chowns the
# resulting files back to the calling user via an alpine helper.

# Anything ending .md in docs/slides/ EXCEPT README.md (which is the
# slide-folder's own README, not a slide source).
SLIDE_SRC := $(filter-out docs/slides/README.md,$(wildcard docs/slides/*.md))
SLIDE_HTML := $(SLIDE_SRC:.md=.html)
SLIDE_PPTX := $(SLIDE_SRC:.md=.pptx)
SLIDE_PDF  := $(SLIDE_SRC:.md=.pdf)

MARP_DOCKER ?= marpteam/marp-cli:latest
SLIDE_UID   := $(shell id -u)
SLIDE_GID   := $(shell id -g)

# Detect a usable local browser; if none, switch to the docker image.
SLIDE_BROWSER := $(or $(CHROME_PATH),$(shell command -v chromium 2>/dev/null),$(shell command -v chromium-browser 2>/dev/null),$(shell command -v google-chrome 2>/dev/null),$(shell command -v firefox 2>/dev/null))

slides: slides-html slides-pptx slides-pdf

slides-html: $(SLIDE_HTML)

slides-pptx: $(SLIDE_PPTX)

slides-pdf: $(SLIDE_PDF)

docs/slides/%.html: docs/slides/%.md
	npx --yes @marp-team/marp-cli $< -o $@

# PPTX/PDF: prefer the local browser (faster, no docker pull). Falls
# back to the marp-cli container when no browser is reachable. The
# container writes files as its internal `marp` user, so we chown back
# to the calling user via an alpine helper afterwards. The temporary
# `chmod 777` on the bind-mounted dir is what lets the in-container
# user write at all; restored to 775 immediately after.
docs/slides/%.pptx: docs/slides/%.md
ifneq ($(SLIDE_BROWSER),)
	npx --yes @marp-team/marp-cli $< -o $@ --allow-local-files
else
	@echo "No local Chromium/Firefox; using $(MARP_DOCKER) for PPTX build"
	rm -f $@
	chmod 777 docs/slides
	docker run --rm --init -v "$(CURDIR)/docs/slides:/home/marp/app" $(MARP_DOCKER) $(notdir $<) --pptx --allow-local-files
	chmod 775 docs/slides
	docker run --rm -v "$(CURDIR)/docs/slides:/work" --user 0:0 alpine chown $(SLIDE_UID):$(SLIDE_GID) /work/$(notdir $@)
endif

docs/slides/%.pdf: docs/slides/%.md
ifneq ($(SLIDE_BROWSER),)
	npx --yes @marp-team/marp-cli $< -o $@ --allow-local-files
else
	@echo "No local Chromium/Firefox; using $(MARP_DOCKER) for PDF build"
	rm -f $@
	chmod 777 docs/slides
	docker run --rm --init -v "$(CURDIR)/docs/slides:/home/marp/app" $(MARP_DOCKER) $(notdir $<) --pdf --allow-local-files
	chmod 775 docs/slides
	docker run --rm -v "$(CURDIR)/docs/slides:/work" --user 0:0 alpine chown $(SLIDE_UID):$(SLIDE_GID) /work/$(notdir $@)
endif
