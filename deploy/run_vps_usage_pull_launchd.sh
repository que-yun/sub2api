#!/bin/zsh
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${ROOT_DIR}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"
LOCK_DIR="${LOCK_DIR:-/tmp/sub2api-vps-usage-pull.lock}"
# 锁目录存在但无真实同步进程时，超过该秒数视为死锁并回收
LOCK_STALE_SECONDS="${LOCK_STALE_SECONDS:-60}"
export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

# 只检测真正执行拉取的进程；不要匹配当前 wrapper，否则死锁永远清不掉
is_pull_running() {
  pgrep -f '/deploy/sync_vps_usage_to_local\.sh' >/dev/null 2>&1
}

acquire_lock() {
  if mkdir "${LOCK_DIR}" 2>/dev/null; then
    return 0
  fi

  if is_pull_running; then
    return 1
  fi

  local lock_age=0
  if [[ -d "${LOCK_DIR}" ]]; then
    lock_age=$(( $(date +%s) - $(stat -f %m "${LOCK_DIR}" 2>/dev/null || echo 0) ))
  fi
  if (( lock_age < LOCK_STALE_SECONDS )); then
    return 1
  fi

  echo "Removing stale VPS usage pull lock (age=${lock_age}s, threshold=${LOCK_STALE_SECONDS}s)"
  rmdir "${LOCK_DIR}" 2>/dev/null || rm -rf "${LOCK_DIR}" 2>/dev/null || true
  mkdir "${LOCK_DIR}" 2>/dev/null
}

if ! acquire_lock; then
  echo "Another VPS usage pull is already running; skip this tick."
  exit 0
fi

# 不能用 exec：否则当前 shell 被替换，EXIT trap 不会释放锁目录
trap 'rmdir "${LOCK_DIR}" 2>/dev/null || true' EXIT

cd "${ROOT_DIR}"
if [[ -f "${ACTIVE_HOST_ENV}" ]]; then set -a; source "${ACTIVE_HOST_ENV}"; set +a; fi
export LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
export LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
export LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5432}}"
export LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-}}"
export SSH_PORT="${SSH_PORT:-22222}"
export SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-${HOME}/.ssh/sub2api_vps_db_tunnel_ed25519}"
export PGUSER="${PGUSER:-${DATABASE_USER:-sub2api}}"
export PGDATABASE="${PGDATABASE:-${DATABASE_DBNAME:-sub2api}}"
export USAGE_PULL_LOOKBACK_HOURS="${USAGE_PULL_LOOKBACK_HOURS:-48}"

"${ROOT_DIR}/deploy/sync_vps_usage_to_local.sh"
