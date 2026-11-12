.PHONY: build build-linux build-windows build-all test fmt clean

BINARY_NAME := library-mcp

build:
	go build -o $(BINARY_NAME) ./cmd/library-mcp

build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY_NAME) ./cmd/library-mcp

build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BINARY_NAME).exe ./cmd/library-mcp

build-all: build-linux build-windows

test:
	go test ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME).exe
