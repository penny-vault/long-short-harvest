BINARY := long-short-harvest

.PHONY: all build test lint clean

all: build

build:
	go build -o $(BINARY) .

test:
	go test ./... -v

lint:
	go vet ./...
	golangci-lint run --fix ./...

clean:
	rm -f $(BINARY)
