#!/bin/zsh
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${ROOT_DIR}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"

export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

cd "${ROOT_DIR}"
if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

export LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
export LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
export LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5432}}"
export LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-}}"
export PGUSER="${PGUSER:-${DATABASE_USER:-sub2api}}"
export PGDATABASE="${PGDATABASE:-${DATABASE_DBNAME:-sub2api}}"
export SSH_PORT="${SSH_PORT:-22222}"
export SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-${HOME}/.ssh/sub2api_vps_db_tunnel_ed25519}"
export SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-8}"
export REMOTE_QUERY_TIMEOUT_SECONDS="${REMOTE_QUERY_TIMEOUT_SECONDS:-45}"
export REMOTE_QUERY_TIMEOUT_SECONDS="${REMOTE_QUERY_TIMEOUT_SECONDS:-90}"

LOCK_DIR="${TMPDIR:-/tmp}/sub2api-vps-grok-oauth-runtime-pull.lock"
if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
  echo "Another VPS Grok OAuth runtime pull is already running; skipping this interval."
  exit 0
fi
trap 'rmdir "${LOCK_DIR}" 2>/dev/null || true' EXIT

"${ROOT_DIR}/deploy/sync_vps_grok_oauth_runtime_to_local.sh"

# VPS 失败观测只会触发本机用自己的出口复探；它不会直接改动本机健康态。
GROK_RECOVER_VPS_OBSERVATION_ONLY=true \
GROK_RECOVER_CONCURRENCY=2 \
GROK_RECOVER_VPS_OBSERVATION_LIMIT=24 \
"${ROOT_DIR}/tools/run_grok_recover_probe.sh"
