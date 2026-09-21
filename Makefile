.PHONY: run run-offline test vet check build dist docker clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w -X github.com/parthiban-sivakumar/gopherdex/internal/version.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

run:
	go run ./cmd/gopherdexd

run-offline:
	go run ./cmd/gopherdexd -offline

test:
	go test ./...

vet:
	go vet ./...

# What CI runs.
check:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go test ./...

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

clean:
	rm -rf bin dist
