.PHONY: build run test fmt

build:
	go build -o build/scheme-simulator .

run:
	go run .

test:
	go test -race ./...

fmt:
	go fmt ./...
