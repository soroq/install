# Reproducible build of the public Soroq CLI. Version = the git tag when built at one
# (so a checkout of a release tag stamps that tag), else the committed ./VERSION baseline.
VERSION := $(shell git describe --tags --exact-match 2>/dev/null || cat VERSION)
LDFLAGS := -X main.buildVersion=$(VERSION)

.PHONY: build test
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o soroq ./cmd/soroq
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o soroqctl ./cmd/soroqctl

test:
	go test ./...
