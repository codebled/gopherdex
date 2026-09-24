.PHONY: run run-offline test vet fmt lint text tidy vuln actions check build dist docker clean bench-seed bench-serve bench-load

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w -X github.com/codebled/gopherdex/internal/version.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

run:
	go run ./cmd/gopherdexd

run-offline:
	go run ./cmd/gopherdexd -offline

# Explicit patterns, not ./...: a benchmark dataset in bench/ holds about a
# million files for the go command to walk.
PKGS := ./cmd/... ./internal/... ./web/...

# Tool versions, pinned so every contributor and CI get the same results.
# `go run pkg@version` fetches and caches them: nothing to install.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.8.0
ACTIONLINT := go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

test:
	go test -race $(PKGS)

vet:
	go vet $(PKGS)

# Formatting: gofmt and goimports. `make fmt` also fixes it.
fmt:
	$(GOLANGCI_LINT) fmt $(PKGS)

lint:
	$(GOLANGCI_LINT) run $(PKGS)

# Trailing whitespace and final newlines in every tracked text file.
text:
	scripts/check-text.sh

# go.mod and go.sum match what the code imports.
tidy:
	go mod tidy -diff

# Known vulnerabilities reachable from our code (needs the network).
vuln:
	$(GOVULNCHECK) $(PKGS)

# GitHub workflow files: inputs, expressions and shell steps.
actions:
	$(ACTIONLINT)

# Everything CI checks, except vuln (network) and the migrations check
# (needs the pull request's base branch). Run it before you push.
check: lint text tidy test

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gopherdexd ./cmd/gopherdexd
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gopherdex ./cmd/gopherdex

# Release binaries for every platform in dist/, with checksums.
dist:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		for cmd in gopherdex gopherdexd; do \
			echo "dist/$$cmd-$(VERSION)-$$os-$$arch$$ext"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
				-o dist/$$cmd-$(VERSION)-$$os-$$arch$$ext ./cmd/$$cmd || exit 1; \
		done; \
	done
	cd dist && shasum -a 256 * > SHA256SUMS

docker:
	docker build --build-arg VERSION=$(VERSION) -t gopherdex:$(VERSION) .

# Load testing (see "Load testing" in README.md). PROFILE=full for 100k modules.
PROFILE ?= small
bench-seed:
	go run ./cmd/gdxbench seed -profile $(PROFILE)

bench-serve:
	go run ./cmd/gopherdexd -db bench/data/gopherdex.db -blobs bench/data/blobs -offline -trust-proxy -notify-mirror=false

bench-load:
	go run ./cmd/gdxbench load -c 32 -d 60s

clean:
	rm -rf bin dist
