VERSION ?= dev
BINARY = torsync
BUILD_FLAGS = -ldflags="-X main.version=$(VERSION)"

.PHONY: build build-all clean run fmt vet

build:
	go build $(BUILD_FLAGS) -o bin/$(BINARY) ./cmd/torsync/

build-all:
	GOOS=linux   GOARCH=amd64 go build $(BUILD_FLAGS) -o bin/$(BINARY)-linux-amd64   ./cmd/torsync/
	GOOS=linux   GOARCH=arm64 go build $(BUILD_FLAGS) -o bin/$(BINARY)-linux-arm64   ./cmd/torsync/
	GOOS=darwin  GOARCH=amd64 go build $(BUILD_FLAGS) -o bin/$(BINARY)-darwin-amd64  ./cmd/torsync/
	GOOS=darwin  GOARCH=arm64 go build $(BUILD_FLAGS) -o bin/$(BINARY)-darwin-arm64  ./cmd/torsync/
	GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o bin/$(BINARY)-windows-amd64.exe ./cmd/torsync/
	GOOS=windows GOARCH=arm64 go build $(BUILD_FLAGS) -o bin/$(BINARY)-windows-arm64.exe ./cmd/torsync/

vet:
	go vet ./...

fmt:
	go fmt ./...

clean:
	rm -rf bin/

run: build
	./bin/$(BINARY) start
