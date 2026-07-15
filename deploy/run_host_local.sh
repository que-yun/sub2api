#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_DIR="${ROOT_DIR}/deploy"
ENV_FILE="${HOST_RUN_ENV_FILE:-${DEPLOY_DIR}/host-run.env}"
OUT_DIR="${HOST_RUN_OUT_DIR:-${DEPLOY_DIR}/out/host}"
BIN_PATH="${OUT_DIR}/sub2api-host"
DIST_INDEX="${ROOT_DIR}/backend/internal/web/dist/index.html"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Missing env file: ${ENV_FILE}" >&2
  echo "Copy ${DEPLOY_DIR}/host-run.env.example to ${DEPLOY_DIR}/host-run.env first." >&2
  exit 1
fi

mkdir -p "${OUT_DIR}"

set -a
source "${ENV_FILE}"
set +a

# 在线更新检查：匿名 GitHub API 仅 60 次/小时，易被本机代理出口共享 IP 打满。
# 优先用 UPDATE_TOKEN；未配置时尝试复用本机 gh 登录态（不落盘、不写 host-run.env）。
if [[ -z "${UPDATE_TOKEN:-}" ]]; then
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    export UPDATE_TOKEN="${GITHUB_TOKEN}"
  elif [[ -n "${GH_TOKEN:-}" ]]; then
    export UPDATE_TOKEN="${GH_TOKEN}"
  elif command -v gh >/dev/null 2>&1; then
    if _gh_tok="$(gh auth token 2>/dev/null)" && [[ -n "${_gh_tok}" ]]; then
      export UPDATE_TOKEN="${_gh_tok}"
    fi
    unset _gh_tok
  fi
fi

: "${DATA_DIR:?DATA_DIR must be set in ${ENV_FILE}}"
mkdir -p "${DATA_DIR}"

if [[ -x "${BIN_PATH}" ]]; then
  echo "Starting host-native sub2api on ${SERVER_HOST:-127.0.0.1}:${SERVER_PORT:-6795}"
  echo "Using DATA_DIR=${DATA_DIR}"
  exec "${BIN_PATH}"
fi

if [[ ! -f "${DIST_INDEX}" ]]; then
  if ! command -v pnpm >/dev/null 2>&1; then
    echo "pnpm is required to build the embedded frontend." >&2
    exit 1
  fi
  (
    cd "${ROOT_DIR}/frontend"
    pnpm install
    pnpm run build
  )
fi

(
  cd "${ROOT_DIR}/backend"
  go build -tags embed -o "${BIN_PATH}" ./cmd/server
)

echo "Starting host-native sub2api on ${SERVER_HOST:-127.0.0.1}:${SERVER_PORT:-6795}"
echo "Using DATA_DIR=${DATA_DIR}"
exec "${BIN_PATH}"
