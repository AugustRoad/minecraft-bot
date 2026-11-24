.PHONY: all build build-bot build-agent test tidy clean

GOOS ?= linux
GOARCH ?= amd64
LDFLAGS := -s -w

all: build

build: build-bot build-agent

build-bot:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mc-bot ./cmd/bot

build-agent:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mc-agent ./cmd/agent

test:
	go test -race -v ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/
