BIN_DIR := bin
BINARY := $(BIN_DIR)/llms-proxy

.PHONY: all build test integration clean

all: test build

build:
	@echo ">> building $(BINARY)"
	@mkdir -p $(BIN_DIR)
	GO111MODULE=on go build -o $(BINARY) ./cmd/proxy

test:
	@echo ">> running unit tests"
	go test ./...

integration:
	@echo ">> running integration tests"
	go test -tags=integration ./test/...

clean:
	@echo ">> cleaning build artifacts"
	rm -rf $(BIN_DIR)
