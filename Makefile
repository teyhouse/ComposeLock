BINARY := composelock
PKG    := ./cmd/composelock

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

IMAGE ?= composelock

FUZZTIME ?= 60s

.PHONY: build run test fuzz smoke smoke-compose-dir lint vuln fmt tidy clean install image

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

run: build
	./bin/$(BINARY) $(ARGS)

test:
	go test -race -shuffle=on ./...

fuzz:
	go test -run '^$$' -fuzz FuzzRedactCredentialURL -fuzztime $(FUZZTIME) ./internal/execx
	go test -run '^$$' -fuzz FuzzRedactInvariants -fuzztime $(FUZZTIME) ./internal/execx

smoke:
	./scripts/smoke.sh

smoke-compose-dir:
	./scripts/smoke-compose-dir.sh

lint:
	go vet ./...
	go tool staticcheck ./...

vuln:
	go tool govulncheck ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -rf bin dist

install: build
	install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)

image:
	docker build \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
