#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
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

TOKEN_URL="${TOKEN_URL:-https://auth.openai.com/oauth/token}"
OPENAI_OAUTH_CLIENT_ID="${OPENAI_OAUTH_CLIENT_ID:-app_EMoamEEZ73f0CkXaXp7hrann}"
OPENAI_OAUTH_REFRESH_SCOPE="${OPENAI_OAUTH_REFRESH_SCOPE:-openid profile email}"
SOURCE_GROUP_NAME="${SOURCE_GROUP_NAME:-codex-warp-source}"
HOT_GROUP_NAME="${HOT_GROUP_NAME:-codex-warp-verified}"
DEAD_GROUP_NAME="${DEAD_GROUP_NAME:-codex-warp-dead}"
MAX_ACCOUNTS="${MAX_ACCOUNTS:-50}"
ACCOUNT_IDS="${ACCOUNT_IDS:-}"
RESULTS_PATH="${RESULTS_PATH:-/tmp/sub2api-openai-oauth-error-recovery-$(date +%Y%m%d-%H%M%S).jsonl}"
CURL_TIMEOUT_SECONDS="${CURL_TIMEOUT_SECONDS:-120}"
SLEEP_SECONDS="${SLEEP_SECONDS:-0}"
DRY_RUN="${DRY_RUN:-false}"
RECOVERY_FAILED_BACKOFF_HOURS="${RECOVERY_FAILED_BACKOFF_HOURS:-168}"

CURL_BIN="${CURL_BIN:-$(command -v curl)}"
JQ_BIN="${JQ_BIN:-$(command -v jq)}"

log_info() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S %z')] $*"
}

sql_escape() {
  printf "%s" "$1" | sed "s/'/''/g"
}

json_line() {
  "$JQ_BIN" -cn \
    --arg account_id "$1" \
    --arg email "$2" \
    --arg outcome "$3" \
    --arg classification "$4" \
    --arg detail "$5" \
    '{account_id: ($account_id|tonumber), email: $email, outcome: $outcome, classification: $classification, detail: $detail}'
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

classify_refresh_error() {
  local body_lc="$1"
  case "$body_lc" in
    *refresh_token_reused*) echo "refresh_token_reused" ;;
    *refresh_token_invalidated*) echo "refresh_token_invalidated" ;;
    *app_session_terminated*) echo "app_session_terminated" ;;
    *invalid_grant*) echo "invalid_grant" ;;
    *invalid_client*) echo "invalid_client" ;;
    *access_denied*) echo "access_denied" ;;
    *timeout*|*timed\ out*) echo "timeout_or_network" ;;
    *) echo "refresh_failed" ;;
  esac
}

recover_account() {
  local account_id="$1"
  local token_json="$2"

  local token_json_sql
  token_json_sql=$(sql_escape "$token_json")

  run_psql -P pager=off -c "
WITH token AS (
  SELECT '${token_json_sql}'::jsonb AS j
),
target AS (
  SELECT
    a.id,
    COALESCE(NULLIF(token.j->>'refresh_token', ''), a.credentials->>'refresh_token') AS refresh_token,
    token.j->>'access_token' AS access_token,
    token.j->>'id_token' AS id_token,
    token.j->>'token_type' AS token_type,
    COALESCE(NULLIF(token.j->>'scope', ''), '${OPENAI_OAUTH_REFRESH_SCOPE}') AS scope,
    COALESCE(NULLIF(token.j->>'expires_in', ''), '3600')::int AS expires_in
  FROM accounts a
  CROSS JOIN token
  WHERE a.id = ${account_id}
    AND a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
),
updated AS (
  UPDATE accounts a
  SET status = 'active',
      schedulable = true,
      error_message = '',
      rate_limited_at = NULL,
      rate_limit_reset_at = NULL,
      overload_until = NULL,
      temp_unschedulable_until = NULL,
      temp_unschedulable_reason = NULL,
      credentials = COALESCE(a.credentials, '{}'::jsonb)
        || jsonb_strip_nulls(jsonb_build_object(
          'access_token', target.access_token,
          'refresh_token', target.refresh_token,
          'id_token', NULLIF(target.id_token, ''),
          'token_type', NULLIF(target.token_type, ''),
          'scope', NULLIF(target.scope, ''),
          'expires_in', target.expires_in::text,
          'expires_at', to_char((NOW() + make_interval(secs => target.expires_in)) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),
          '_token_version', (extract(epoch from now()) * 1000)::bigint
        )),
      updated_at = NOW()
  FROM target
  WHERE a.id = target.id
    AND NULLIF(target.access_token, '') IS NOT NULL
  RETURNING a.id
),
dead_group AS (
  SELECT id FROM groups WHERE name = '$(sql_escape "$DEAD_GROUP_NAME")' AND deleted_at IS NULL LIMIT 1
),
hot_group AS (
  SELECT id FROM groups WHERE name = '$(sql_escape "$HOT_GROUP_NAME")' AND deleted_at IS NULL LIMIT 1
),
removed_dead AS (
  DELETE FROM account_groups ag
  USING updated u, dead_group dg
  WHERE ag.account_id = u.id
    AND ag.group_id = dg.id
  RETURNING ag.account_id
),
inserted_hot AS (
  INSERT INTO account_groups(account_id, group_id, priority, created_at)
  SELECT u.id, hg.id, 1, NOW()
  FROM updated u CROSS JOIN hot_group hg
  ON CONFLICT (account_id, group_id) DO NOTHING
  RETURNING account_id
)
SELECT 'recovered=' || (SELECT count(*) FROM updated)
  || ' removed_dead=' || (SELECT count(*) FROM removed_dead)
  || ' inserted_hot=' || (SELECT count(*) FROM inserted_hot);
"
}

mark_recovery_failure() {
  local account_id="$1"
  local classification="$2"
  local detail="$3"

  local classification_sql detail_sql
  classification_sql=$(sql_escape "$classification")
  detail_sql=$(sql_escape "${detail:0:300}")

  run_psql -P pager=off -c "
UPDATE accounts
SET temp_unschedulable_until = CASE
      WHEN ${RECOVERY_FAILED_BACKOFF_HOURS} > 0 THEN NOW() + make_interval(hours => ${RECOVERY_FAILED_BACKOFF_HOURS})
      ELSE temp_unschedulable_until
    END,
    temp_unschedulable_reason = 'oauth error recovery failed: ${classification_sql}; ${detail_sql}',
    updated_at = NOW()
WHERE id = ${account_id}
  AND deleted_at IS NULL
  AND platform = 'openai'
  AND type = 'oauth';
"
}

candidate_limit_sql="$MAX_ACCOUNTS"
if [[ "$candidate_limit_sql" != <-> ]]; then
  echo "MAX_ACCOUNTS must be an integer, got: $MAX_ACCOUNTS" >&2
  exit 1
fi

account_ids_filter_sql=""
if [[ -n "$ACCOUNT_IDS" ]]; then
  account_ids_filter_sql="AND a.id = ANY(string_to_array('$(sql_escape "$ACCOUNT_IDS")', ',')::bigint[])"
fi

log_info "recovery start max=${MAX_ACCOUNTS} dry_run=${DRY_RUN} results=${RESULTS_PATH}"
: > "$RESULTS_PATH"

CANDIDATES=$(
  run_psql -At -F $'\t' -P pager=off -c "
WITH candidates AS (
  SELECT DISTINCT
    a.id,
    COALESCE(a.credentials->>'email', a.name, '') AS email,
    a.credentials->>'refresh_token' AS refresh_token,
    COALESCE(NULLIF(a.credentials->>'client_id', ''), '${OPENAI_OAUTH_CLIENT_ID}') AS client_id,
    CASE
      WHEN p.id IS NULL THEN ''
      WHEN COALESCE(p.username, '') <> '' AND COALESCE(p.password, '') <> ''
        THEN p.protocol || '://' || p.username || ':' || p.password || '@' || p.host || ':' || p.port
      ELSE p.protocol || '://' || p.host || ':' || p.port
    END AS proxy_url
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups sg ON sg.id = ag.group_id
  LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL AND p.status = 'active'
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND sg.name IN ('$(sql_escape "$SOURCE_GROUP_NAME")', '$(sql_escape "$DEAD_GROUP_NAME")')
    AND sg.deleted_at IS NULL
    AND NULLIF(a.credentials->>'refresh_token', '') IS NOT NULL
    AND (
      a.status = 'error'
      OR a.schedulable = false
      OR COALESCE(a.error_message, '') <> ''
      OR EXISTS (
        SELECT 1
        FROM account_groups dag
        JOIN groups dg ON dg.id = dag.group_id
        WHERE dag.account_id = a.id
          AND dg.name = '$(sql_escape "$DEAD_GROUP_NAME")'
          AND dg.deleted_at IS NULL
      )
    )
    AND (
      COALESCE(a.temp_unschedulable_reason, '') NOT ILIKE 'oauth error recovery failed:%'
      OR a.temp_unschedulable_until IS NULL
      OR a.temp_unschedulable_until <= NOW()
    )
    ${account_ids_filter_sql}
)
SELECT id, email, refresh_token, client_id, proxy_url
FROM candidates
ORDER BY id
LIMIT ${candidate_limit_sql};
"
)

if [[ -z "$CANDIDATES" ]]; then
  log_info "no candidates"
  exit 0
fi

success=0
failed=0
while IFS=$'\t' read -r account_id email refresh_token client_id proxy_url; do
  [[ -z "$account_id" ]] && continue
  log_info "account=${account_id} email=${email} refreshing"

  if [[ "$DRY_RUN" == "true" ]]; then
    json_line "$account_id" "$email" "dry_run" "" "" >> "$RESULTS_PATH"
    continue
  fi

  curl_args=(
    -sS
    --max-time "$CURL_TIMEOUT_SECONDS"
    -w $'\n%{http_code}'
    -H "User-Agent: codex-cli/0.91.0"
    -d "grant_type=refresh_token"
    --data-urlencode "refresh_token=${refresh_token}"
    --data-urlencode "client_id=${client_id}"
    --data-urlencode "scope=${OPENAI_OAUTH_REFRESH_SCOPE}"
  )
  if [[ -n "$proxy_url" ]]; then
    curl_args+=(--proxy "$proxy_url")
  fi

  response="$("$CURL_BIN" "${curl_args[@]}" "$TOKEN_URL" 2>&1 || true)"
  http_code="$(printf '%s' "$response" | tail -n 1)"
  body="$(printf '%s' "$response" | sed '$d')"

  if [[ "$http_code" == "200" ]] && printf '%s' "$body" | "$JQ_BIN" -e '.access_token | strings | length > 0' >/dev/null 2>&1; then
    recover_account "$account_id" "$body" >/dev/null
    json_line "$account_id" "$email" "recovered" "" "" >> "$RESULTS_PATH"
    echo "account=${account_id} recovered"
    success=$((success + 1))
  else
    body_lc="$(printf '%s' "$body" | tr '[:upper:]' '[:lower:]')"
    classification="$(classify_refresh_error "$body_lc")"
    detail="$(printf '%s' "$body" | tr '\n' ' ' | cut -c 1-500)"
    mark_recovery_failure "$account_id" "$classification" "http=${http_code} ${detail}" >/dev/null
    json_line "$account_id" "$email" "failed" "$classification" "http=${http_code} ${detail}" >> "$RESULTS_PATH"
    echo "account=${account_id} failed classification=${classification} http=${http_code}"
    failed=$((failed + 1))
  fi

  if [[ "$SLEEP_SECONDS" != "0" ]]; then
    sleep "$SLEEP_SECONDS"
  fi
done <<< "$CANDIDATES"

log_info "recovery done success=${success} failed=${failed} results=${RESULTS_PATH}"
