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
export SSH_PORT="${SSH_PORT:-22222}"
export SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-${HOME}/.ssh/sub2api_vps_db_tunnel_ed25519}"
export PGUSER="${PGUSER:-${DATABASE_USER:-sub2api}}"
export PGDATABASE="${PGDATABASE:-${DATABASE_DBNAME:-sub2api}}"
export STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-6}"
export STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-false}"
export STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
export STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
export STANDBY_RESTART_AFTER_TOKEN_SYNC="${STANDBY_RESTART_AFTER_TOKEN_SYNC:-false}"
exec "${ROOT_DIR}/deploy/sync_openai_oauth_tokens_to_vps.sh"
