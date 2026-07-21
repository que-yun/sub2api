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
export STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-false}"
export STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
export STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
export STANDBY_RESTART_AFTER_SCHEDULING_SYNC="${STANDBY_RESTART_AFTER_SCHEDULING_SYNC:-false}"
export STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS="${STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS:-true}"
export STANDBY_RESTART_AFTER_ROUTE_SYNC="${STANDBY_RESTART_AFTER_ROUTE_SYNC:-false}"
export STANDBY_REMAP_API_KEY_GROUP_PAIRS="${STANDBY_REMAP_API_KEY_GROUP_PAIRS:-}"

export REMOTE_RETRIES="${REMOTE_RETRIES:-5}"
export SSH_RETRIES="${SSH_RETRIES:-5}"
export SCHEDULING_ROUTE_COOLDOWN_SECONDS="${SCHEDULING_ROUTE_COOLDOWN_SECONDS:-12}"
export ROUTE_SYNC_RETRIES="${ROUTE_SYNC_RETRIES:-3}"
export ROUTE_SYNC_RETRY_SLEEP_SECONDS="${ROUTE_SYNC_RETRY_SLEEP_SECONDS:-15}"

"${ROOT_DIR}/deploy/sync_openai_oauth_scheduling_to_vps.sh"

# Route upload is lighter but still sensitive to SSH flakes right after the
# large scheduling scp/COPY burst. Cool down, then retry route independently
# so a transient Connection closed does not discard a successful scheduling push.
route_attempt=1
while true; do
  sleep "${SCHEDULING_ROUTE_COOLDOWN_SECONDS}"
  if "${ROOT_DIR}/deploy/sync_route_config_to_vps.sh"; then
    break
  fi
  rc=$?
  if (( route_attempt >= ROUTE_SYNC_RETRIES )); then
    echo "Route configuration sync failed after ${route_attempt} attempts, exit=${rc}" >&2
    exit "${rc}"
  fi
  echo "Route configuration sync failed (exit=${rc}), retry ${route_attempt}/${ROUTE_SYNC_RETRIES} in ${ROUTE_SYNC_RETRY_SLEEP_SECONDS}s ..." >&2
  sleep "${ROUTE_SYNC_RETRY_SLEEP_SECONDS}"
  route_attempt=$((route_attempt + 1))
done
