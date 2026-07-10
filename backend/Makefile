# Reproducible build of the public Soroq CLI. `make build` stamps the version from ./VERSION.
VERSION := $(shell cat VERSION)
LDFLAGS := -X main.buildVersion=$(VERSION)

.PHONY: build test
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o soroq ./cmd/soroq
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o soroqctl ./cmd/soroqctl

test:
	go test ./...
