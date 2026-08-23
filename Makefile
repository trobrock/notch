VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

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
