.PHONY: build fmt vet

build:
	go build -trimpath -o bin/hfproxy ./cmd/hfproxy

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...
