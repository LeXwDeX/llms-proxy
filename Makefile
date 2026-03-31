BIN_DIR := bin
BINARY := $(BIN_DIR)/llms-proxy
CATALOG_RAW := /tmp/models_dev_raw.json
CATALOG_OUT := internal/catalog/data/models.json

.PHONY: all build test integration clean catalog

all: test build

catalog:
	@echo ">> updating model catalog from models.dev"
	@if curl -sSf --connect-timeout 10 --max-time 30 https://models.dev/api.json -o $(CATALOG_RAW) 2>/dev/null; then \
		python3 scripts/update-model-catalog.py $(CATALOG_RAW) $(CATALOG_OUT) && \
		echo ">> catalog updated successfully"; \
	else \
		echo ">> catalog download failed, keeping existing data"; \
	fi

build: catalog
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
