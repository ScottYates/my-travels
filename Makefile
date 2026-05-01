.PHONY: build clean test

# Default output: ./my-travels in repo root.
# Override with: make build OUT=/home/exedev/srv
OUT ?= ./my-travels

build:
	go build -o $(OUT) ./cmd/srv

clean:
	rm -f my-travels

test:
	go test ./...
