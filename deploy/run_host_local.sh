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
