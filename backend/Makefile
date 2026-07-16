.PHONY: build generate test test-unit test-integration test-e2e

VERSION ?= $(shell ./scripts/resolve-version.sh)
LDFLAGS ?= -s -w -X main.Version=$(VERSION)
DIST_INDEX ?= internal/web/dist/index.html

# 始终 -tags embed。dist 缺失时失败，避免再次产出无前端二进制。
build:
	@if [ ! -f "$(DIST_INDEX)" ]; then \
		echo "Frontend dist missing: $(DIST_INDEX)" >&2; \
		echo "Build frontend first: make -C .. build-frontend  (or: make -C .. build)" >&2; \
		exit 1; \
	fi
	CGO_ENABLED=0 go build -tags embed -ldflags="$(LDFLAGS)" -trimpath -o bin/server ./cmd/server

generate:
	go generate ./ent
	go generate ./cmd/server

test:
	go test ./...
	golangci-lint run ./...

test-unit:
	go test -tags=unit ./...

test-integration:
	go test -tags=integration ./...

test-e2e:
	./scripts/e2e-test.sh

test-e2e-local:
	go test -tags=e2e -v -timeout=300s ./internal/integration/...
