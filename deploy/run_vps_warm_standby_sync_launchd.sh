#!/bin/zsh
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${ROOT_DIR}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"
LOCK_DIR="${LOCK_DIR:-/tmp/sub2api-vps-warm-standby-sync.lock}"
LOCK_MAX_AGE_SECONDS="${LOCK_MAX_AGE_SECONDS:-14400}"

export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
  lock_age_seconds="$(( $(date +%s) - $(stat -f %m "${LOCK_DIR}" 2>/dev/null || echo 0) ))"
  if [[ "${lock_age_seconds}" -gt "${LOCK_MAX_AGE_SECONDS}" ]] && ! pgrep -f "sync_to_vps_warm_standby.sh" >/dev/null 2>&1; then
    echo "Removing stale VPS warm standby sync lock; age=${lock_age_seconds}s."
    rm -rf "${LOCK_DIR}"
    mkdir "${LOCK_DIR}"
  else
    echo "Another VPS warm standby sync is already running; skip this tick."
    exit 0
  fi
fi
trap 'rmdir "${LOCK_DIR}" 2>/dev/null || true' EXIT

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
export SYNC_MODE="${SYNC_MODE:-full}"
export START_STANDBY_AFTER_SYNC="${START_STANDBY_AFTER_SYNC:-true}"
export PULL_STANDBY_OPENAI_OAUTH_TOKENS="${PULL_STANDBY_OPENAI_OAUTH_TOKENS:-false}"
export PULL_STANDBY_RUNTIME_RECORDS="${PULL_STANDBY_RUNTIME_RECORDS:-false}"
export STANDBY_RUNTIME_PULL_LOOKBACK_HOURS="${STANDBY_RUNTIME_PULL_LOOKBACK_HOURS:-48}"
export STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-6}"
export STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-false}"
export STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS="${STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS:-false}"
export STANDBY_CLEAR_GROK_PROXY_IDS="${STANDBY_CLEAR_GROK_PROXY_IDS:-false}"
export STANDBY_CLEAR_LOCALHOST_PROXY_IDS="${STANDBY_CLEAR_LOCALHOST_PROXY_IDS:-13}"
export STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
export STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
export STANDBY_LOCAL_PROXY_NAME="${STANDBY_LOCAL_PROXY_NAME:-vps-mihomo-socks5-7891}"
export STANDBY_LOCAL_PROXY_PROTOCOL="${STANDBY_LOCAL_PROXY_PROTOCOL:-socks5}"
export STANDBY_LOCAL_PROXY_HOST="${STANDBY_LOCAL_PROXY_HOST:-127.0.0.1}"
export STANDBY_LOCAL_PROXY_PORT="${STANDBY_LOCAL_PROXY_PORT:-7891}"
export STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS="${STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS:-false}"
export STANDBY_RUN_MODE="${STANDBY_RUN_MODE:-standard}"
export STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED="${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED:-false}"
export STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED="${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED:-false}"
export STANDBY_RESTORE_STRATEGY="${STANDBY_RESTORE_STRATEGY:-shadow_swap}"
export STANDBY_KEEP_PREVIOUS_DBS="${STANDBY_KEEP_PREVIOUS_DBS:-1}"
# 不能用 exec：exec 会替换掉当前 shell，导致第 23 行的 EXIT trap 无法触发、
# LOCK_DIR 每次成功后都残留，把实际同步周期从每小时拖成 ~4 小时（stale 阈值）。
# 用普通调用，wrapper 作为父进程存活到子脚本结束后再跑 trap 清锁。
"${ROOT_DIR}/deploy/sync_to_vps_warm_standby.sh"
