#!/usr/bin/env bash
# 内部实现：由仓库根目录 `make run` 或 LaunchAgent 调用。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_DIR="${ROOT_DIR}/deploy"
ENV_FILE="${HOST_RUN_ENV_FILE:-${DEPLOY_DIR}/host-run.env}"
OUT_DIR="${HOST_RUN_OUT_DIR:-${DEPLOY_DIR}/out/host}"
BIN_PATH="${OUT_DIR}/sub2api-host"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Missing env file: ${ENV_FILE}" >&2
  echo "Prepare ${DEPLOY_DIR}/host-run.env first." >&2
  exit 1
fi

mkdir -p "${OUT_DIR}"

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

# 在线更新检查：优先 UPDATE_TOKEN / GITHUB_TOKEN / GH_TOKEN / gh auth token
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

need_build=false
if [[ "${FORCE_REBUILD:-false}" == "true" ]]; then
  need_build=true
elif [[ ! -x "${BIN_PATH}" ]]; then
  need_build=true
elif command -v strings >/dev/null 2>&1; then
  # NOTE: do not use grep -q under pipefail — early exit SIGPIPEs strings and
  # makes a successful marker match look like a failed pipeline (! becomes true).
  if strings "${BIN_PATH}" | grep -F "Frontend not embedded" >/dev/null; then
    echo "Existing binary is non-embed; rebuilding host binary."
    need_build=true
  elif ! strings "${BIN_PATH}" | grep -F "__CSP_NONCE_VALUE__" >/dev/null; then
    echo "Existing binary missing frontend markers; rebuilding host binary."
    need_build=true
  fi
fi

if [[ "${need_build}" == "true" ]]; then
  make -C "${ROOT_DIR}" build-host
fi

echo "Starting host-native sub2api on ${SERVER_HOST:-127.0.0.1}:${SERVER_PORT:-6795}"
echo "Using DATA_DIR=${DATA_DIR}"
exec "${BIN_PATH}"
