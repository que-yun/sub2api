#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${REPO_ROOT}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"

export PATH="/usr/local/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

export HOST_RUN_ENV="${HOST_RUN_ENV:-${ACTIVE_HOST_ENV}}"
export SUB2API_BASE_URL="${SUB2API_BASE_URL:-http://${SERVER_HOST:-127.0.0.1}:${SERVER_PORT:-6780}}"
export GROK_RECOVER_OUT_DIR="${GROK_RECOVER_OUT_DIR:-${ACTIVE_DEPLOY_DIR}/data-host/grok-scan/recover}"
export GROK_PREFERRED_PROXY_ID="${GROK_PREFERRED_PROXY_ID:-13}"
# 30-min launchd tick; sticky 403 only every 6h inside the script.
export GROK_RECOVER_CONCURRENCY="${GROK_RECOVER_CONCURRENCY:-4}"
export GROK_RECOVER_FAST_LIMIT="${GROK_RECOVER_FAST_LIMIT:-80}"
export GROK_RECOVER_403_LIMIT="${GROK_RECOVER_403_LIMIT:-120}"
export GROK_RECOVER_403_INTERVAL_SEC="${GROK_RECOVER_403_INTERVAL_SEC:-21600}"

mkdir -p "${GROK_RECOVER_OUT_DIR}"
LOCK_DIR="${TMPDIR:-/tmp}/sub2api-grok-recover-probe.lock"
if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
  echo "Another Grok recovery probe is already running; skipping this interval."
  exit 0
fi
trap 'rmdir "${LOCK_DIR}" 2>/dev/null || true' EXIT

/usr/bin/env python3 "${SCRIPT_DIR}/grok_recover_probe.py"
