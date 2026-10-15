VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cuijianzhuang/polaris/internal/panel.Version=$(VERSION) -X github.com/cuijianzhuang/polaris/internal/agent.Version=$(VERSION)

.PHONY: all web build agent test test-cores proto lint clean

all: web build

web:
	cd web && npm ci --no-audit --no-fund && npm run build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/polaris ./cmd/panel
	$(MAKE) agent

agent:
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o bin/polaris-agent-linux-$$arch ./cmd/agent; \
	done

test:
	go vet ./...
	go test ./...

# Runs adapter, subscription and end-to-end tests against real core binaries:
#   make test-cores XRAY=/path/to/xray SINGBOX=/path/to/sing-box
test-cores:
	POLARIS_XRAY_BIN=$(XRAY) POLARIS_SINGBOX_BIN=$(SINGBOX) go test -count=1 ./...

proto:
	buf generate

lint:
	cd web && npm run typecheck
	gofmt -l . | (! grep .)

clean:
	rm -rf bin web/dist/assets web/dist/index.html
