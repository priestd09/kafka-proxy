.DEFAULT_GOAL := build

.PHONY: clean build build.docker tag all

BINARY        ?= kafka-proxy
SOURCES        = $(shell find . -name '*.go' | grep -v /vendor/)
VERSION       ?= $(shell git describe --tags --always --dirty)
GOPKGS         = $(shell go list ./... | grep -v /vendor/)
BUILD_FLAGS   ?=
LDFLAGS       ?= -X github.com/grepplabs/kafka-proxy/config.Version=$(VERSION) -w -s
TAG           ?= "v0.0.8"
GOARCH        ?= amd64
GOOS          ?= linux

default: build

test.race:
	GOCACHE=off go test -v -race `go list ./...`

test:
	GOCACHE=off go test -v `go list ./...`

fmt:
	go fmt $(GOPKGS)

check:
	golint $(GOPKGS)
	go vet $(GOPKGS)


build: build/$(BINARY)

build/$(BINARY): $(SOURCES)
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o build/$(BINARY) $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" .

docker.build:
	docker build --build-arg GOOS=$(GOOS) --build-arg  GOARCH=$(GOARCH) -f Dockerfile.build .

tag:
	git tag $(TAG)

release: clean
	git push origin $(TAG)
	curl -sL https://git.io/goreleaser | bash

protoc.local-auth:
	protoc -I plugin/local-auth/proto/ plugin/local-auth/proto/auth.proto --go_out=plugins=grpc:plugin/local-auth/proto/

protoc.gateway-client:
	protoc -I plugin/gateway-client/proto/ plugin/gateway-client/proto/token-provider.proto --go_out=plugins=grpc:plugin/gateway-client/proto/

protoc.gateway-server:
	protoc -I plugin/gateway-server/proto/ plugin/gateway-server/proto/token-info.proto --go_out=plugins=grpc:plugin/gateway-server/proto/

plugin.auth-user:
	CGO_ENABLED=0 go build -o build/auth-user $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" cmd/plugin-auth-user/main.go

plugin.auth-ldap:
	CGO_ENABLED=0 go build -o build/auth-ldap $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" cmd/plugin-auth-ldap/main.go

plugin.google-id-provider:
	CGO_ENABLED=0 go build -o build/google-id-provider $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" cmd/plugin-googleid-provider/main.go

plugin.google-id-info:
	CGO_ENABLED=0 go build -o build/google-id-info $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" cmd/plugin-googleid-info/main.go

all: build plugin.auth-user plugin.auth-ldap plugin.google-id-provider plugin.google-id-info

clean:
	@rm -rf build
