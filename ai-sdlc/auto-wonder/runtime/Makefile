.PHONY: build test fmt vet check docker

build:
	go build -trimpath -o bin/autowonder-runtime ./cmd/autowonder-runtime

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

check: fmt test vet

docker:
	docker build -t autowonder-runtime:local .
