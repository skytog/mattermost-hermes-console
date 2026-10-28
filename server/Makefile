PLUGIN_ID := com.nicox.hermes-console
PLUGIN_VERSION := $(shell cat plugin.json | python3 -c "import sys,json; print(json.load(sys.stdin)['version'])")
BUNDLE_NAME := $(PLUGIN_ID)-$(PLUGIN_VERSION).tar.gz

GO ?= go
DIST_DIR := dist
BIN_DIR := $(DIST_DIR)/server/dist
PLATFORMS := linux-amd64 linux-arm64

.PHONY: all build webapp package clean test tidy

all: package

## Build server binaries (Mattermost host is linux-amd64; arm64 for portability).
build:
	@mkdir -p $(BIN_DIR)
	@for platform in $(PLATFORMS); do \
		os=$$(echo $$platform | cut -d- -f1); arch=$$(echo $$platform | cut -d- -f2); \
		echo "  Building $$platform..."; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			$(GO) build -trimpath -o $(BIN_DIR)/plugin-$$platform . ; \
	done

## Build the webapp bundle (React/TS) with esbuild.
webapp:
	@cd ../webapp && { [ -d node_modules ] || npm install --no-audit --no-fund; } && npm run build

## Package into a distributable tar.gz (plugin.json + server/ + webapp/dist).
package: build webapp
	@cp plugin.json $(DIST_DIR)/
	@mkdir -p $(DIST_DIR)/webapp
	@cp -r ../webapp/dist $(DIST_DIR)/webapp/dist
	@cd $(DIST_DIR) && tar -czf ../$(DIST_DIR)/$(BUNDLE_NAME) server/ webapp/ plugin.json
	@rm -rf $(BIN_DIR) $(DIST_DIR)/plugin.json $(DIST_DIR)/webapp
	@echo "Bundle: $(DIST_DIR)/$(BUNDLE_NAME)"

clean:
	@rm -rf $(DIST_DIR)

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy
