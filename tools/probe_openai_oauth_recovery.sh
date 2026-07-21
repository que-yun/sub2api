#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SYNC_SCRIPT="${SCRIPT_DIR}/sync_openai_oauth_pool.sh"
DEPLOY_ENV_FILE="${DEPLOY_ENV_FILE:-${REPO_ROOT}/deploy/.env}"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${REPO_ROOT}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"

if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5432}}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-${PGPASSWORD:-}}}"
DB_CONTAINER="${DB_CONTAINER:-sub2api-postgres}"
DB_USER="${DB_USER:-${DATABASE_USER:-sub2api}}"
DB_NAME="${DB_NAME:-${DATABASE_DBNAME:-sub2api}}"

SERVER_HOST="${SERVER_HOST:-127.0.0.1}"
SERVER_PORT="${SERVER_PORT:-6780}"
BASE_URL="${BASE_URL:-http://${SERVER_HOST}:${SERVER_PORT}}"
ADMIN_EMAIL="${ADMIN_EMAIL:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
MODEL_ID="${MODEL_ID:-gpt-5.5}"
PROBE_TIMEOUT_SECONDS="${PROBE_TIMEOUT_SECONDS:-90}"
PROBE_RETRY_COOLDOWN_SECONDS="${PROBE_RETRY_COOLDOWN_SECONDS:-900}"
ENABLE_SCHEDULING_ON_RECOVERY="${ENABLE_SCHEDULING_ON_RECOVERY:-true}"
SOURCE_GROUP_NAME="${SOURCE_GROUP_NAME:-codex-warp-source}"
INITIAL_PROBE_REASON="${INITIAL_PROBE_REASON:-local pool pending initial probe}"

CURL_BIN="${CURL_BIN:-$(command -v curl)}"
JQ_BIN="${JQ_BIN:-$(command -v jq)}"

log_info() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S %z')] $*"
}

run_psql() {
  case "${LOCAL_PG_SOURCE}" in
    host)
      if [[ -z "${LOCAL_PG_PASSWORD}" ]]; then
        echo "LOCAL_PG_PASSWORD or PGPASSWORD is required when LOCAL_PG_SOURCE=host" >&2
        exit 1
      fi
      PGPASSWORD="${LOCAL_PG_PASSWORD}" psql -h "${LOCAL_PG_HOST}" -p "${LOCAL_PG_PORT}" -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 "$@"
      ;;
    docker)
      docker exec -i "$DB_CONTAINER" psql -v ON_ERROR_STOP=1 -U "$DB_USER" -d "$DB_NAME" "$@"
      ;;
    *)
      echo "Unsupported LOCAL_PG_SOURCE=${LOCAL_PG_SOURCE}. Use host or docker." >&2
      exit 1
      ;;
  esac
}

sql_escape() {
  printf "%s" "$1" | sed "s/'/''/g"
}

has_accounts_runtime_columns() {
  local result
  result=$(
    run_psql -At -F $'\t' -P pager=off -c "
SELECT COUNT(*)
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = 'accounts'
  AND column_name IN ('temp_unschedulable_until', 'temp_unschedulable_reason');
"
  )
  [[ "$result" == "2" ]]
}

env_value() {
  local key="$1"
  if [[ ! -f "$DEPLOY_ENV_FILE" ]]; then
    return 1
  fi
  awk -F= -v key="$key" '$1 == key { print substr($0, index($0, "=") + 1) }' "$DEPLOY_ENV_FILE" | tail -n 1
}

if [[ -z "$ADMIN_EMAIL" ]]; then
  ADMIN_EMAIL="$(env_value ADMIN_EMAIL || true)"
fi

if [[ -z "$ADMIN_PASSWORD" ]]; then
  ADMIN_PASSWORD="$(env_value ADMIN_PASSWORD || true)"
fi

if [[ -z "$ADMIN_EMAIL" || -z "$ADMIN_PASSWORD" ]]; then
  echo "missing ADMIN_EMAIL or ADMIN_PASSWORD" >&2
  exit 1
fi

if ! has_accounts_runtime_columns; then
  log_info "probe skip: accounts runtime columns missing, waiting for sub2api migrations"
  exit 0
fi

if ! "$CURL_BIN" -fsS --max-time 3 "${BASE_URL}/health" >/dev/null 2>&1; then
  log_info "probe skip: sub2api health unavailable at ${BASE_URL}"
  exit 0
fi

SOURCE_GROUP_NAME_SQL=$(sql_escape "$SOURCE_GROUP_NAME")
INITIAL_PROBE_REASON_SQL=$(sql_escape "$INITIAL_PROBE_REASON")

get_access_token() {
  local response token
  response=$(
    "$CURL_BIN" -sS --fail-with-body \
      -X POST "${BASE_URL}/api/v1/auth/login" \
      -H 'Content-Type: application/json' \
      -d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PASSWORD}\",\"turnstile_token\":\"\"}"
  )
  token=$(printf '%s' "$response" | "$JQ_BIN" -r '.data.access_token // empty')
  if [[ -z "$token" ]]; then
    echo "admin login did not return access token" >&2
    exit 1
  fi
  printf '%s\n' "$token"
}

apply_probe_backoff() {
  local account_id="$1"
  local reason="$2"
  local reason_short reason_sql
  reason_short="${reason:0:220}"
  reason_sql=$(sql_escape "local pool probe failed: ${reason_short}")

  run_psql -c "
UPDATE accounts
SET temp_unschedulable_until = GREATEST(
      COALESCE(temp_unschedulable_until, NOW()),
      NOW() + make_interval(secs => ${PROBE_RETRY_COOLDOWN_SECONDS})
    ),
    temp_unschedulable_reason = '${reason_sql}'
WHERE id = ${account_id}
  AND deleted_at IS NULL;
"
}

clear_initial_probe_mark() {
  local account_id="$1"

  run_psql -c "
UPDATE accounts
SET temp_unschedulable_until = NULL,
    temp_unschedulable_reason = NULL
WHERE id = ${account_id}
  AND deleted_at IS NULL
  AND COALESCE(temp_unschedulable_reason, '') = '${INITIAL_PROBE_REASON_SQL}';
"
}

recover_schedulable_after_success() {
  local account_id="$1"

  if [[ "${ENABLE_SCHEDULING_ON_RECOVERY}" != "true" ]]; then
    return 0
  fi

  run_psql -c "
UPDATE accounts
SET schedulable = true,
    temp_unschedulable_until = NULL,
    temp_unschedulable_reason = NULL,
    updated_at = NOW()
WHERE id = ${account_id}
  AND deleted_at IS NULL
  AND platform = 'openai'
  AND type = 'oauth'
  AND status = 'active'
  AND COALESCE(error_message, '') = ''
  AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at <= NOW())
  AND (overload_until IS NULL OR overload_until <= NOW())
  AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until <= NOW());
"
}

probe_account() {
  local token="$1"
  local account_id="$2"
  local sse events error success

  if ! sse=$(
    "$CURL_BIN" -sS -N --max-time "$PROBE_TIMEOUT_SECONDS" \
      -X POST "${BASE_URL}/api/v1/admin/accounts/${account_id}/test" \
      -H "Authorization: Bearer ${token}" \
      -H 'Content-Type: application/json' \
      -d "{\"model_id\":\"${MODEL_ID}\"}"
  ); then
    apply_probe_backoff "$account_id" "probe request failed"
    echo "account=${account_id} probe_request_failed"
    return 1
  fi

  events=$(printf '%s\n' "$sse" | sed -n 's/^data: //p')
  success=$(printf '%s\n' "$events" | "$JQ_BIN" -r 'select(.type == "test_complete" and .success == true) | "true"' 2>/dev/null | tail -n 1)
  error=$(printf '%s\n' "$events" | "$JQ_BIN" -r 'select(.type == "error") | .error' 2>/dev/null | tail -n 1)

  if [[ "$success" == "true" ]]; then
    clear_initial_probe_mark "$account_id"
    recover_schedulable_after_success "$account_id"
    echo "account=${account_id} recovered"
    return 0
  fi

  if [[ -z "$error" ]]; then
    error="probe ended without success or error event"
  fi

  case "$error" in
    *"API returned 429"*|*"usage_limit_reached"*)
      echo "account=${account_id} still_rate_limited"
      ;;
    *"Authentication failed (401)"*|*"API returned 401"*|*"token revoked"*|*"invalidated"*)
      echo "account=${account_id} auth_invalid"
      ;;
    *)
      apply_probe_backoff "$account_id" "$error"
      echo "account=${account_id} probe_backoff"
      ;;
  esac

  return 1
}

log_info "probe start"

ACCESS_TOKEN="$(get_access_token)"

CANDIDATES=$(
  run_psql -At -F $'\t' -P pager=off -c "
WITH due_accounts AS (
  SELECT DISTINCT
    a.id,
    a.rate_limit_reset_at,
    a.temp_unschedulable_until,
    a.temp_unschedulable_reason
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups sg ON sg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND a.status = 'active'
    AND COALESCE(a.error_message, '') NOT ILIKE '%token revoked%'
    AND COALESCE(a.error_message, '') NOT ILIKE '%authentication token has been invalidated%'
    AND sg.name = '${SOURCE_GROUP_NAME_SQL}'
    AND sg.deleted_at IS NULL
    AND (
      COALESCE(a.temp_unschedulable_reason, '') = '${INITIAL_PROBE_REASON_SQL}'
      OR (
        a.rate_limit_reset_at IS NOT NULL
        AND a.rate_limit_reset_at <= NOW()
        AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW())
      )
      OR (
        a.schedulable = false
        AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW())
        AND (a.overload_until IS NULL OR a.overload_until <= NOW())
        AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW())
        AND COALESCE(a.temp_unschedulable_reason, '') NOT ILIKE '%token revoked%'
        AND COALESCE(a.temp_unschedulable_reason, '') NOT ILIKE '%authentication token has been invalidated%'
        AND COALESCE(a.temp_unschedulable_reason, '') NOT ILIKE '%refresh_token_reused%'
      )
    )
)
SELECT
  id,
  COALESCE(rate_limit_reset_at::text, ''),
  COALESCE(temp_unschedulable_until::text, '')
FROM due_accounts
ORDER BY
  CASE WHEN COALESCE(temp_unschedulable_reason, '') = '${INITIAL_PROBE_REASON_SQL}' THEN 0 ELSE 1 END,
  rate_limit_reset_at NULLS LAST,
  id;
"
)

if [[ -z "$CANDIDATES" ]]; then
  echo "no due accounts to probe"
  "$SYNC_SCRIPT"
  log_info "probe done"
  exit 0
fi

while IFS=$'\t' read -r account_id rate_limit_reset_at temp_unschedulable_until; do
  [[ -z "$account_id" ]] && continue
  echo "account=${account_id} due rate_limit_reset_at=${rate_limit_reset_at} temp_unschedulable_until=${temp_unschedulable_until}"
  probe_account "$ACCESS_TOKEN" "$account_id" || true
done <<< "$CANDIDATES"

"$SYNC_SCRIPT"

log_info "probe done"
