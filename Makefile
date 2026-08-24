VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: build test check install example-plugin clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/notch ./cmd/notch

test:
	go test ./...

check:
	go test -race ./...
	go vet ./...

install:
	CGO_ENABLED=0 go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/notch

example-plugin:
	CGO_ENABLED=0 go build -trimpath -o examples/extensions/hello-plugin/hello-plugin ./examples/extensions/hello-plugin

clean:
	rm -rf bin examples/extensions/hello-plugin/hello-plugin
