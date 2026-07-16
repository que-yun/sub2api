# =============================================================================
# 唯一入口（后来人只记这里）
#   make              帮助
#   make build        一键编译前后端（强制 embed）
#   make run          本机启动
#   make deploy       编译并推 VPS
# =============================================================================

.DEFAULT_GOAL := help

.PHONY: help build run deploy \
	build-backend build-frontend build-host build-linux \
	verify-embed ensure-frontend-dist \
	test test-backend test-frontend test-frontend-critical

FRONTEND_CRITICAL_VITEST := \
	src/views/auth/__tests__/LinuxDoCallbackView.spec.ts \
	src/views/auth/__tests__/WechatCallbackView.spec.ts \
	src/views/user/__tests__/PaymentView.spec.ts \
	src/views/user/__tests__/PaymentResultView.spec.ts \
	src/components/user/profile/__tests__/ProfileInfoCard.spec.ts \
	src/views/admin/__tests__/SettingsView.spec.ts

DIST_INDEX := backend/internal/web/dist/index.html
HOST_OUT_DIR := deploy/out/host
LINUX_OUT_DIR := deploy/out/linux-amd64
HOST_BIN := $(HOST_OUT_DIR)/sub2api-host
LINUX_BIN := $(LINUX_OUT_DIR)/sub2api
BACKEND_BIN := backend/bin/server

VERSION ?= $(shell ./backend/scripts/resolve-version.sh)
LDFLAGS ?= -s -w -X main.Version=$(VERSION)

help:
	@echo "sub2api 唯一入口"
	@echo ""
	@echo "  make build     一键编译：前端 + 本机二进制 + linux/amd64 二进制"
	@echo "  make run       本机启动（自动补 embed 二进制）"
	@echo "  make deploy    编译并推 VPS standby，校验 /health 与 / 页面"
	@echo ""
	@echo "可选："
	@echo "  make test"
	@echo "  FORCE_REBUILD=true make run"
	@echo "  FORCE_FRONTEND_BUILD=true make build"

# -----------------------------------------------------------------------------
# 对外
# -----------------------------------------------------------------------------

build: build-frontend build-backend build-host build-linux
	@echo "Build completed (frontend embedded)."
	@echo "Next: make run  或  make deploy"

run:
	@./deploy/run_host_local.sh

deploy: build-linux
	@./deploy/deploy_vps_binary.sh

# -----------------------------------------------------------------------------
# 内部
# -----------------------------------------------------------------------------

build-host: ensure-frontend-dist
	@mkdir -p "$(HOST_OUT_DIR)"
	@echo "Building host binary (embed) -> $(HOST_BIN)"
	@cd backend && CGO_ENABLED=0 go build -tags embed -ldflags="$(LDFLAGS)" -trimpath -o "../$(HOST_BIN)" ./cmd/server
	@$(MAKE) verify-embed BIN="$(HOST_BIN)"

build-linux: ensure-frontend-dist
	@mkdir -p "$(LINUX_OUT_DIR)"
	@echo "Building linux/amd64 binary (embed) -> $(LINUX_BIN)"
	@cd backend && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags embed -ldflags="$(LDFLAGS)" -trimpath -o "../$(LINUX_BIN)" ./cmd/server
	@$(MAKE) verify-embed BIN="$(LINUX_BIN)"

build-backend: ensure-frontend-dist
	@$(MAKE) -C backend build
	@$(MAKE) verify-embed BIN="$(BACKEND_BIN)"

build-frontend:
	@pnpm --dir frontend run build
	@test -f "$(DIST_INDEX)" || (echo "Frontend dist missing after build: $(DIST_INDEX)" >&2; exit 1)
	@echo "Frontend ready: $(DIST_INDEX)"

ensure-frontend-dist:
	@if [ "$(FORCE_FRONTEND_BUILD)" = "true" ] || [ ! -f "$(DIST_INDEX)" ]; then \
		$(MAKE) build-frontend; \
	else \
		echo "Using existing frontend dist: $(DIST_INDEX)"; \
	fi

verify-embed:
	@test -n "$(BIN)" || (echo "verify-embed requires BIN=..." >&2; exit 1)
	@test -x "$(BIN)" || (echo "Binary missing or not executable: $(BIN)" >&2; exit 1)
	@if command -v strings >/dev/null 2>&1; then \
		if strings "$(BIN)" | grep -F -q "Frontend not embedded"; then \
			echo "Embed check failed: $(BIN) still contains non-embed message." >&2; \
			exit 1; \
		fi; \
		if ! strings "$(BIN)" | grep -F -q "__CSP_NONCE_VALUE__"; then \
			echo "Embed check failed: $(BIN) missing frontend CSP placeholder." >&2; \
			exit 1; \
		fi; \
	else \
		echo "Warning: strings not found; skipped binary content embed checks." >&2; \
	fi
	@size=$$(wc -c < "$(BIN)" | tr -d ' '); \
	if [ "$$size" -lt 80000000 ]; then \
		echo "Embed check warning: $(BIN) size=$$size looks unusually small for embed build." >&2; \
	fi
	@echo "Embed verified: $(BIN)"
	@if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$(BIN)"; \
	elif command -v sha256sum >/dev/null 2>&1; then sha256sum "$(BIN)"; fi

# -----------------------------------------------------------------------------
# 测试
# -----------------------------------------------------------------------------

test: test-backend test-frontend

test-backend:
	@$(MAKE) -C backend test

test-frontend:
	@pnpm --dir frontend run lint:check
	@pnpm --dir frontend run typecheck
	@$(MAKE) test-frontend-critical

test-frontend-critical:
	@pnpm --dir frontend exec vitest run $(FRONTEND_CRITICAL_VITEST)
