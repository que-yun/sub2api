#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_DIR="${ROOT_DIR}/deploy"
ENV_FILE="${GZIP_FRONT_PROXY_ENV_FILE:-${DEPLOY_DIR}/gzip-front-proxy.env}"
OUT_DIR="${GZIP_FRONT_PROXY_OUT_DIR:-${DEPLOY_DIR}/out/host}"
BIN_PATH="${OUT_DIR}/gzip-front-proxy"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Missing env file: ${ENV_FILE}" >&2
  echo "Copy ${DEPLOY_DIR}/gzip-front-proxy.env.example to ${DEPLOY_DIR}/gzip-front-proxy.env first." >&2
  exit 1
fi

mkdir -p "${OUT_DIR}"

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

if [[ "${GZIP_FRONT_PROXY_SKIP_BUILD:-false}" == "true" && -x "${BIN_PATH}" ]]; then
  echo "Using existing binary ${BIN_PATH} (GZIP_FRONT_PROXY_SKIP_BUILD=true)"
else
  (
    cd "${ROOT_DIR}/backend"
    go build -o "${BIN_PATH}" ./cmd/gzipfrontproxy
  )
fi

echo "Starting gzip front proxy on ${GZIP_FRONT_PROXY_LISTEN:-127.0.0.1:6791}"
echo "Upstream ${GZIP_FRONT_PROXY_UPSTREAM:-http://127.0.0.1:6780}"
exec "${BIN_PATH}"
