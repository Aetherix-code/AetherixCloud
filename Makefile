# AetherixCloud. CGO is disabled so the embedded registry builds on macOS too.
export CGO_ENABLED ?= 0
BIN ?= bin
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test test-integration test-e2e vet fmt proto proto-web web web-dev-deps dev dev-image dev-cluster dev-down dev-daemon tidy clean

all: vet test build

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/ax ./cmd/ax
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/aetherixd ./cmd/aetherixd

test:
	go test ./...

# Docker-in-Docker multi-node suite (see docs/testing.md). Skips if image missing.
test-integration:
	go test -tags=integration -count=1 -timeout 20m ./internal/devcluster/

# Playwright mock-mode dashboard flows.
test-e2e: web-dev-deps
	cd web && npx playwright install --with-deps chromium >/dev/null 2>&1 || npx playwright install chromium
	cd web && npm run test:e2e

vet:
	go vet ./...

fmt:
	gofmt -s -w cmd internal

tidy:
	go mod tidy

# Regenerate protobuf/Connect code into gen/ (local plugins in .bin/).
proto:
	PATH="$(CURDIR)/.bin:$$PATH" .bin/buf generate

# TypeScript clients for the dashboard (committed in web/src/gen).
proto-web: web-dev-deps
	.bin/buf generate --template buf.gen.web.yaml

web-dev-deps:
	@test -d web/node_modules || (cd web && npm ci)

# Build the SPA and copy it into internal/web/dist for go:embed.
web: web-dev-deps
	cd web && npm run build
	find internal/web/dist -mindepth 1 ! -name '.gitkeep' -delete 2>/dev/null || true
	cp -R web/dist/. internal/web/dist/

# Vite dev server with hot reload; /aetherix.v1.* etc. proxy to node 1
# (AX_PROXY, default http://127.0.0.1:8080). Use `make dev MOCK=1` for mock data.
AX_PROXY ?= http://127.0.0.1:8080
dev: web-dev-deps
	cd web && AX_PROXY=$(AX_PROXY) VITE_MOCK=$(MOCK) npm run dev

dev-image:
	docker build -f deploy/dev/Dockerfile.node -t aetherix/node:dev .

# 3-node local cluster (Docker-in-Docker).
dev-cluster: build
	$(BIN)/ax dev up --build

dev-down: build
	$(BIN)/ax dev down --volumes

# Run a single-node daemon on the host without WireGuard/Docker/Corrosion.
dev-daemon: build
	$(BIN)/aetherixd init --data-dir .dev-data --advertise 127.0.0.1 || true
	$(BIN)/aetherixd run --data-dir .dev-data --store sqlite --no-mesh --no-docker

clean:
	rm -rf $(BIN) .dev-data web/dist
