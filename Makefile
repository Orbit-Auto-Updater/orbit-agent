VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARCHES  := amd64 arm64 arm 386

.PHONY: build release test vet devserver clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/linux-dash-agent ./cmd/linux-dash-agent

# Static Linux binaries for every supported architecture, plus checksums.
release:
	@mkdir -p dist
	@for arch in $(ARCHES); do \
		echo "building linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/linux-dash-agent-linux-$$arch ./cmd/linux-dash-agent || exit 1; \
	done
	@cp deploy/install.sh dist/
	@echo "$(VERSION)" > dist/VERSION
	@cd dist && (sha256sum linux-dash-agent-linux-* 2>/dev/null || shasum -a 256 linux-dash-agent-linux-*) > SHA256SUMS

test:
	go test -race ./...

vet:
	go vet ./...
	GOOS=linux go vet ./...

devserver:
	go run ./cmd/devserver

clean:
	rm -rf bin dist
