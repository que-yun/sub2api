#!/bin/zsh
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${ROOT_DIR}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"
LOCK_DIR="${LOCK_DIR:-${TMPDIR:-/tmp}/sub2api-vps-openai-oauth-scheduling-sync.lock}"
LOCK_MAX_AGE_SECONDS="${LOCK_MAX_AGE_SECONDS:-1800}"

export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

cd "${ROOT_DIR}"
if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
  lock_age_seconds="$(( $(date +%s) - $(stat -f %m "${LOCK_DIR}" 2>/dev/null || echo 0) ))"
  if [[ "${lock_age_seconds}" -gt "${LOCK_MAX_AGE_SECONDS}" ]] && ! pgrep -f "sync_(openai_oauth_scheduling_to_vps|route_config_to_vps).sh" >/dev/null 2>&1; then
    echo "Removing stale VPS scheduling/route sync lock; age=${lock_age_seconds}s."
    rm -rf "${LOCK_DIR}"
    mkdir "${LOCK_DIR}"
  else
    echo "Another VPS scheduling/route sync is already running; skipping this interval."
    exit 0
  fi
fi
trap 'rmdir "${LOCK_DIR}" 2>/dev/null || true' EXIT

export LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
export LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
export LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5432}}"
export LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-}}"
export PGUSER="${PGUSER:-${DATABASE_USER:-sub2api}}"
export PGDATABASE="${PGDATABASE:-${DATABASE_DBNAME:-sub2api}}"
export SSH_PORT="${SSH_PORT:-22222}"
export SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-${HOME}/.ssh/sub2api_vps_db_tunnel_ed25519}"
export STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-6}"
export STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-true}"
export STANDBY_RESTART_AFTER_SCHEDULING_SYNC="${STANDBY_RESTART_AFTER_SCHEDULING_SYNC:-false}"
export STANDBY_RESTART_AFTER_ROUTE_SYNC="${STANDBY_RESTART_AFTER_ROUTE_SYNC:-false}"
export STANDBY_REMAP_API_KEY_GROUP_PAIRS="${STANDBY_REMAP_API_KEY_GROUP_PAIRS:-}"

"${ROOT_DIR}/deploy/sync_openai_oauth_scheduling_to_vps.sh"
"${ROOT_DIR}/deploy/sync_route_config_to_vps.sh"
