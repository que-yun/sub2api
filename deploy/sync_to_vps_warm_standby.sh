#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
REMOTE_COPY_HOST="${REMOTE_COPY_HOST:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-15}"
VPS_TAILSCALE_IP="${VPS_TAILSCALE_IP:-100.99.28.61}"
STANDBY_SERVER_PORT="${STANDBY_SERVER_PORT:-6781}"

REMOTE_DIR="${REMOTE_DIR:-/opt/sub2api-standby}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"
LOCAL_PG_CONTAINER="${LOCAL_PG_CONTAINER:-sub2api-postgres}"
LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-127.0.0.1}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-5432}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${PGPASSWORD:-}}"
PGUSER="${PGUSER:-sub2api}"
PGDATABASE="${PGDATABASE:-sub2api}"
SYNC_MODE="${SYNC_MODE:-full}"
KEEP_LOCAL_DUMP="${KEEP_LOCAL_DUMP:-false}"
START_STANDBY_AFTER_SYNC="${START_STANDBY_AFTER_SYNC:-false}"
STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-}"
STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-false}"
STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS="${STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS:-false}"
STANDBY_CLEAR_GROK_PROXY_IDS="${STANDBY_CLEAR_GROK_PROXY_IDS:-false}"
STANDBY_CLEAR_LOCALHOST_PROXY_IDS="${STANDBY_CLEAR_LOCALHOST_PROXY_IDS:-13}"
# VPS 代理策略：
# - remap_local: 保留“需要代理”的账户，但统一映射到 VPS 本机 mihomo
# - clear_all: 全部清空为直连（旧行为）
# - keep: 不做额外处理（不推荐，会把本机 127.0.0.1 代理原样带过去）
STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
STANDBY_LOCAL_PROXY_NAME="${STANDBY_LOCAL_PROXY_NAME:-vps-mihomo-socks5-7891}"
STANDBY_LOCAL_PROXY_PROTOCOL="${STANDBY_LOCAL_PROXY_PROTOCOL:-socks5}"
STANDBY_LOCAL_PROXY_HOST="${STANDBY_LOCAL_PROXY_HOST:-127.0.0.1}"
STANDBY_LOCAL_PROXY_PORT="${STANDBY_LOCAL_PROXY_PORT:-7891}"
STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS="${STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS:-false}"

# remap_local 时不能先按平台清空 proxy，否则后面无东西可 remap。
if [[ "${STANDBY_PROXY_POLICY}" == "remap_local" ]]; then
  STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS=false
  STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS=false
  STANDBY_CLEAR_GROK_PROXY_IDS=false
  STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS=false
fi
PULL_STANDBY_OPENAI_OAUTH_TOKENS="${PULL_STANDBY_OPENAI_OAUTH_TOKENS:-false}"
PULL_STANDBY_RUNTIME_RECORDS="${PULL_STANDBY_RUNTIME_RECORDS:-false}"
STANDBY_RUNTIME_PULL_LOOKBACK_HOURS="${STANDBY_RUNTIME_PULL_LOOKBACK_HOURS:-48}"
STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED="${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED:-}"
STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED="${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED:-}"
STANDBY_RESTORE_STRATEGY="${STANDBY_RESTORE_STRATEGY:-shadow_swap}"
STANDBY_KEEP_PREVIOUS_DBS="${STANDBY_KEEP_PREVIOUS_DBS:-1}"

timestamp="$(date +%Y%m%d-%H%M%S)"
timestamp_id="${timestamp//-/_}"
tmp_dir="$(mktemp -d)"
dump_path="${tmp_dir}/sub2api-${SYNC_MODE}-${timestamp}.dump"
remote_dump="${REMOTE_DIR}/backups/$(basename "${dump_path}")"
# Shadow swaps restore local configuration into a new DB. The runtime snapshot below is
# captured from the active VPS DB only after traffic has stopped, then merged into that DB.
standby_anthropic_runtime_path="/tmp/sub2api-anthropic-setup-runtime-${timestamp_id}.tsv"

cleanup() {
  if [[ "${KEEP_LOCAL_DUMP}" != "true" ]]; then
    rm -rf "${tmp_dir}"
  else
    echo "Local dump kept at: ${dump_path}"
  fi
}
trap cleanup EXIT

SSH_RETRIES="${SSH_RETRIES:-4}"
SSH_RETRY_SLEEP_SECONDS="${SSH_RETRY_SLEEP_SECONDS:-8}"

remote_exec() {
  local attempt=1
  local rc=0
  while true; do
    ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" "$@" && return 0
    rc=$?
    if (( attempt >= SSH_RETRIES )); then
      return "${rc}"
    fi
    echo "remote_exec failed (exit=${rc}), retry ${attempt}/${SSH_RETRIES} in ${SSH_RETRY_SLEEP_SECONDS}s ..." >&2
    sleep "${SSH_RETRY_SLEEP_SECONDS}"
    attempt=$((attempt + 1))
  done
}

remote_copy() {
  local attempt=1
  local rc=0
  while true; do
    gzip -c "$1" | ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "gzip -dc > $(shell_quote "$2")" \
      && ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "test -s $(shell_quote "$2")" && return 0
    rc=$?
    if (( attempt >= SSH_RETRIES )); then
      return "${rc}"
    fi
    echo "remote_copy failed (exit=${rc}), retry ${attempt}/${SSH_RETRIES} in ${SSH_RETRY_SLEEP_SECONDS}s ..." >&2
    sleep "${SSH_RETRY_SLEEP_SECONDS}"
    attempt=$((attempt + 1))
  done
}

shell_quote() {
  printf "%q" "$1"
}

sql_literal() {
  printf "'%s'" "${1//\'/\'\'}"
}

local_psql() {
  case "${LOCAL_PG_SOURCE}" in
    docker)
      docker exec "${LOCAL_PG_CONTAINER}" psql -U "${PGUSER}" -d "${PGDATABASE}" "$@"
      ;;
    host)
      if [[ -z "${LOCAL_PG_PASSWORD}" ]]; then
        echo "LOCAL_PG_PASSWORD or PGPASSWORD is required when LOCAL_PG_SOURCE=host" >&2
        exit 1
      fi
      PGPASSWORD="${LOCAL_PG_PASSWORD}" psql -h "${LOCAL_PG_HOST}" -p "${LOCAL_PG_PORT}" -U "${PGUSER}" -d "${PGDATABASE}" "$@"
      ;;
    *)
      echo "Unsupported LOCAL_PG_SOURCE=${LOCAL_PG_SOURCE}. Use docker or host." >&2
      exit 1
      ;;
  esac
}

local_pg_dump() {
  case "${LOCAL_PG_SOURCE}" in
    docker)
      docker exec "${LOCAL_PG_CONTAINER}" pg_dump \
        -U "${PGUSER}" \
        -d "${PGDATABASE}" \
        "$@"
      ;;
    host)
      if [[ -z "${LOCAL_PG_PASSWORD}" ]]; then
        echo "LOCAL_PG_PASSWORD or PGPASSWORD is required when LOCAL_PG_SOURCE=host" >&2
        exit 1
      fi
      PGPASSWORD="${LOCAL_PG_PASSWORD}" pg_dump \
        -h "${LOCAL_PG_HOST}" \
        -p "${LOCAL_PG_PORT}" \
        -U "${PGUSER}" \
        -d "${PGDATABASE}" \
        "$@"
      ;;
    *)
      echo "Unsupported LOCAL_PG_SOURCE=${LOCAL_PG_SOURCE}. Use docker or host." >&2
      exit 1
      ;;
  esac
}

ssh_args=(
  -o BatchMode=yes
  -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}"
  -o ServerAliveInterval=15
  -o ServerAliveCountMax=4
  -o StrictHostKeyChecking=accept-new
)
if [[ -n "${SSH_PORT}" ]]; then
  ssh_args+=(-p "${SSH_PORT}")
fi
if [[ -n "${SSH_IDENTITY_FILE}" ]]; then
  ssh_args+=(-i "${SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes)
fi

pull_standby_openai_oauth_tokens() {
  local pull_dump="${tmp_dir}/standby-openai-oauth-credentials.tsv"

  echo "Pulling fresher OpenAI OAuth credentials from VPS standby ..."
  remote_exec "docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    id,
    credentials,
    updated_at
  FROM public.accounts
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'oauth'
    AND credentials ? 'refresh_token'
    AND credentials ? 'access_token'
    AND credentials ? 'expires_at'
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\\t', QUOTE E'\\b');
\"" > "${pull_dump}"

  local row_count
  row_count="$(wc -l < "${pull_dump}" | tr -d ' ')"
  if [[ "${row_count}" == "0" ]]; then
    echo "No standby OpenAI OAuth credentials to pull."
    return 0
  fi

  echo "Merging ${row_count} standby OpenAI OAuth credential rows into local database when fresher ..."
  if [[ "${LOCAL_PG_SOURCE}" == "docker" ]]; then
    docker cp "${pull_dump}" "${LOCAL_PG_CONTAINER}:/tmp/standby-openai-oauth-credentials.tsv"
    local_psql -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE standby_openai_oauth_credentials (
  id bigint PRIMARY KEY,
  credentials jsonb NOT NULL,
  updated_at timestamptz NOT NULL
);

\copy standby_openai_oauth_credentials (id, credentials, updated_at) FROM '/tmp/standby-openai-oauth-credentials.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH candidates AS (
  SELECT
    a.id,
    s.credentials,
    s.updated_at,
    CASE
      WHEN NULLIF(a.credentials->>'expires_at', '') ~ '^[0-9]+$'
        THEN to_timestamp((a.credentials->>'expires_at')::double precision)
      WHEN NULLIF(a.credentials->>'expires_at', '') IS NOT NULL
        THEN (a.credentials->>'expires_at')::timestamptz
      ELSE NULL
    END AS local_expires_at,
    CASE
      WHEN NULLIF(s.credentials->>'expires_at', '') ~ '^[0-9]+$'
        THEN to_timestamp((s.credentials->>'expires_at')::double precision)
      WHEN NULLIF(s.credentials->>'expires_at', '') IS NOT NULL
        THEN (s.credentials->>'expires_at')::timestamptz
      ELSE NULL
    END AS standby_expires_at
  FROM public.accounts a
  JOIN standby_openai_oauth_credentials s ON s.id = a.id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND s.credentials ? 'refresh_token'
    AND s.credentials ? 'access_token'
    AND s.credentials ? 'expires_at'
),
updated AS (
  UPDATE public.accounts a
  SET credentials = c.credentials,
      updated_at = GREATEST(a.updated_at, c.updated_at)
  FROM candidates c
  WHERE a.id = c.id
    AND c.standby_expires_at IS NOT NULL
    AND (c.local_expires_at IS NULL OR c.standby_expires_at > c.local_expires_at)
  RETURNING a.id
)
SELECT 'pulled_standby_openai_oauth_accounts=' || count(*) FROM updated;
SQL
    docker exec "${LOCAL_PG_CONTAINER}" rm -f /tmp/standby-openai-oauth-credentials.tsv
  else
    local_psql -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE standby_openai_oauth_credentials (
  id bigint PRIMARY KEY,
  credentials jsonb NOT NULL,
  updated_at timestamptz NOT NULL
);

\\copy standby_openai_oauth_credentials (id, credentials, updated_at) FROM '${pull_dump}' WITH (FORMAT csv, DELIMITER E'\\t', QUOTE E'\\b');

WITH candidates AS (
  SELECT
    a.id,
    s.credentials,
    s.updated_at,
    CASE
      WHEN NULLIF(a.credentials->>'expires_at', '') ~ '^[0-9]+$'
        THEN to_timestamp((a.credentials->>'expires_at')::double precision)
      WHEN NULLIF(a.credentials->>'expires_at', '') IS NOT NULL
        THEN (a.credentials->>'expires_at')::timestamptz
      ELSE NULL
    END AS local_expires_at,
    CASE
      WHEN NULLIF(s.credentials->>'expires_at', '') ~ '^[0-9]+$'
        THEN to_timestamp((s.credentials->>'expires_at')::double precision)
      WHEN NULLIF(s.credentials->>'expires_at', '') IS NOT NULL
        THEN (s.credentials->>'expires_at')::timestamptz
      ELSE NULL
    END AS standby_expires_at
  FROM public.accounts a
  JOIN standby_openai_oauth_credentials s ON s.id = a.id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND s.credentials ? 'refresh_token'
    AND s.credentials ? 'access_token'
    AND s.credentials ? 'expires_at'
),
updated AS (
  UPDATE public.accounts a
  SET credentials = c.credentials,
      updated_at = GREATEST(a.updated_at, c.updated_at)
  FROM candidates c
  WHERE a.id = c.id
    AND c.standby_expires_at IS NOT NULL
    AND (c.local_expires_at IS NULL OR c.standby_expires_at > c.local_expires_at)
  RETURNING a.id
)
SELECT 'pulled_standby_openai_oauth_accounts=' || count(*) FROM updated;
SQL
  fi
}

pull_standby_runtime_records() {
  local usage_dump="${tmp_dir}/standby-usage-logs.tsv"
  local error_dump="${tmp_dir}/standby-ops-error-logs.tsv"
  local touch_dump="${tmp_dir}/standby-last-used.tsv"
  local lookback_hours_sql
  lookback_hours_sql="$(sql_literal "${STANDBY_RUNTIME_PULL_LOOKBACK_HOURS}")"

  echo "Pulling recent runtime records from VPS standby (lookback=${STANDBY_RUNTIME_PULL_LOOKBACK_HOURS}h) ..."

  remote_exec "docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    user_id, api_key_id, account_id, request_id, model,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    cache_creation_5m_tokens, cache_creation_1h_tokens,
    input_cost, output_cost, cache_creation_cost, cache_read_cost,
    total_cost, actual_cost, stream, duration_ms, created_at,
    group_id, subscription_id, rate_multiplier, first_token_ms,
    billing_type, user_agent, image_count, image_size, ip_address,
    account_rate_multiplier, reasoning_effort, cache_ttl_overridden,
    openai_ws_mode, request_type, service_tier, inbound_endpoint,
    upstream_endpoint, upstream_model, requested_model, channel_id,
    model_mapping_chain, billing_tier, billing_mode,
    image_output_tokens, image_output_cost, ingress_node, account_stats_cost
  FROM public.usage_logs
  WHERE created_at >= now() - make_interval(hours => (${lookback_hours_sql})::int)
  ORDER BY created_at, id
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${usage_dump}"

  remote_exec "docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    request_id, client_request_id, user_id, api_key_id, account_id, group_id,
    client_ip, platform, model, request_path, stream, user_agent,
    error_phase, error_type, severity, status_code, is_business_limited,
    error_message, error_body, error_source, error_owner, account_status,
    upstream_status_code, upstream_error_message, upstream_error_detail,
    provider_error_code, provider_error_type, network_error_type,
    retry_after_seconds, duration_ms, time_to_first_token_ms,
    auth_latency_ms, routing_latency_ms, upstream_latency_ms,
    response_latency_ms, created_at, upstream_errors, is_count_tokens,
    resolved, resolved_at, resolved_by_user_id, inbound_endpoint, upstream_endpoint,
    requested_model, upstream_model, request_type,
    attempted_key_prefix, deleted_key_owner_user_id, deleted_key_name, api_key_prefix
  FROM public.ops_error_logs
  WHERE created_at >= now() - make_interval(hours => (${lookback_hours_sql})::int)
  ORDER BY created_at, id
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${error_dump}"

  remote_exec "docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT 'account'::text AS kind, id, last_used_at
  FROM public.accounts
  WHERE deleted_at IS NULL AND last_used_at IS NOT NULL
  UNION ALL
  SELECT 'api_key'::text AS kind, id, last_used_at
  FROM public.api_keys
  WHERE deleted_at IS NULL AND last_used_at IS NOT NULL
  ORDER BY kind, id
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${touch_dump}"

  echo "Merging standby runtime records into local database ..."
  local_psql -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE standby_usage_logs AS
SELECT
  user_id, api_key_id, account_id, request_id, model,
  input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
  cache_creation_5m_tokens, cache_creation_1h_tokens,
  input_cost, output_cost, cache_creation_cost, cache_read_cost,
  total_cost, actual_cost, stream, duration_ms, created_at,
  group_id, subscription_id, rate_multiplier, first_token_ms,
  billing_type, user_agent, image_count, image_size, ip_address,
  account_rate_multiplier, reasoning_effort, cache_ttl_overridden,
  openai_ws_mode, request_type, service_tier, inbound_endpoint,
  upstream_endpoint, upstream_model, requested_model, channel_id,
  model_mapping_chain, billing_tier, billing_mode,
  image_output_tokens, image_output_cost, ingress_node, account_stats_cost
FROM public.usage_logs
WHERE false;

\\copy standby_usage_logs FROM '${usage_dump}' WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');

WITH inserted AS (
  INSERT INTO public.usage_logs (
    user_id, api_key_id, account_id, request_id, model,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    cache_creation_5m_tokens, cache_creation_1h_tokens,
    input_cost, output_cost, cache_creation_cost, cache_read_cost,
    total_cost, actual_cost, stream, duration_ms, created_at,
    group_id, subscription_id, rate_multiplier, first_token_ms,
    billing_type, user_agent, image_count, image_size, ip_address,
    account_rate_multiplier, reasoning_effort, cache_ttl_overridden,
    openai_ws_mode, request_type, service_tier, inbound_endpoint,
    upstream_endpoint, upstream_model, requested_model, channel_id,
    model_mapping_chain, billing_tier, billing_mode,
    image_output_tokens, image_output_cost, ingress_node, account_stats_cost
  )
  SELECT
    t.user_id, t.api_key_id, t.account_id, t.request_id, t.model,
    t.input_tokens, t.output_tokens, t.cache_creation_tokens, t.cache_read_tokens,
    t.cache_creation_5m_tokens, t.cache_creation_1h_tokens,
    t.input_cost, t.output_cost, t.cache_creation_cost, t.cache_read_cost,
    t.total_cost, t.actual_cost, t.stream, t.duration_ms, t.created_at,
    t.group_id, t.subscription_id, t.rate_multiplier, t.first_token_ms,
    t.billing_type, t.user_agent, t.image_count, t.image_size, t.ip_address,
    t.account_rate_multiplier, t.reasoning_effort, t.cache_ttl_overridden,
    t.openai_ws_mode, t.request_type, t.service_tier, t.inbound_endpoint,
    t.upstream_endpoint, t.upstream_model, t.requested_model, t.channel_id,
    t.model_mapping_chain, t.billing_tier, t.billing_mode,
    t.image_output_tokens, t.image_output_cost, t.ingress_node, t.account_stats_cost
  FROM standby_usage_logs t
  WHERE NOT EXISTS (
    SELECT 1
    FROM public.usage_logs u
    WHERE u.api_key_id = t.api_key_id
      AND u.account_id = t.account_id
      AND u.created_at = t.created_at
      AND u.request_id IS NOT DISTINCT FROM t.request_id
  )
  ON CONFLICT (request_id, api_key_id) DO NOTHING
  RETURNING id
)
SELECT 'pulled_standby_usage_logs=' || count(*) FROM inserted;

CREATE TEMP TABLE standby_ops_error_logs AS
SELECT
  request_id, client_request_id, user_id, api_key_id, account_id, group_id,
  client_ip, platform, model, request_path, stream, user_agent,
  error_phase, error_type, severity, status_code, is_business_limited,
  error_message, error_body, error_source, error_owner, account_status,
  upstream_status_code, upstream_error_message, upstream_error_detail,
  provider_error_code, provider_error_type, network_error_type,
  retry_after_seconds, duration_ms, time_to_first_token_ms,
  auth_latency_ms, routing_latency_ms, upstream_latency_ms,
  response_latency_ms, created_at, upstream_errors, is_count_tokens,
  resolved, resolved_at, resolved_by_user_id, inbound_endpoint, upstream_endpoint,
  requested_model, upstream_model, request_type,
  attempted_key_prefix, deleted_key_owner_user_id, deleted_key_name, api_key_prefix
FROM public.ops_error_logs
WHERE false;

\\copy standby_ops_error_logs FROM '${error_dump}' WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');

WITH inserted AS (
  INSERT INTO public.ops_error_logs (
    request_id, client_request_id, user_id, api_key_id, account_id, group_id,
    client_ip, platform, model, request_path, stream, user_agent,
    error_phase, error_type, severity, status_code, is_business_limited,
    error_message, error_body, error_source, error_owner, account_status,
    upstream_status_code, upstream_error_message, upstream_error_detail,
    provider_error_code, provider_error_type, network_error_type,
    retry_after_seconds, duration_ms, time_to_first_token_ms,
    auth_latency_ms, routing_latency_ms, upstream_latency_ms,
    response_latency_ms, created_at, upstream_errors, is_count_tokens,
    resolved, resolved_at, resolved_by_user_id, inbound_endpoint, upstream_endpoint,
    requested_model, upstream_model, request_type,
    attempted_key_prefix, deleted_key_owner_user_id, deleted_key_name, api_key_prefix
  )
  SELECT
    t.request_id, t.client_request_id, t.user_id, t.api_key_id, t.account_id, t.group_id,
    t.client_ip, t.platform, t.model, t.request_path, t.stream, t.user_agent,
    t.error_phase, t.error_type, t.severity, t.status_code, t.is_business_limited,
    t.error_message, t.error_body, t.error_source, t.error_owner, t.account_status,
    t.upstream_status_code, t.upstream_error_message, t.upstream_error_detail,
    t.provider_error_code, t.provider_error_type, t.network_error_type,
    t.retry_after_seconds, t.duration_ms, t.time_to_first_token_ms,
    t.auth_latency_ms, t.routing_latency_ms, t.upstream_latency_ms,
    t.response_latency_ms, t.created_at, t.upstream_errors, t.is_count_tokens,
    t.resolved, t.resolved_at, t.resolved_by_user_id, t.inbound_endpoint, t.upstream_endpoint,
    t.requested_model, t.upstream_model, t.request_type,
    t.attempted_key_prefix, t.deleted_key_owner_user_id, t.deleted_key_name, t.api_key_prefix
  FROM standby_ops_error_logs t
  WHERE NOT EXISTS (
    SELECT 1
    FROM public.ops_error_logs e
    WHERE e.created_at = t.created_at
      AND e.request_id IS NOT DISTINCT FROM t.request_id
      AND e.api_key_id IS NOT DISTINCT FROM t.api_key_id
      AND e.account_id IS NOT DISTINCT FROM t.account_id
      AND e.error_phase = t.error_phase
      AND e.error_type = t.error_type
  )
  RETURNING id
)
SELECT 'pulled_standby_ops_error_logs=' || count(*) FROM inserted;

CREATE TEMP TABLE standby_last_used (
  kind text NOT NULL,
  id bigint NOT NULL,
  last_used_at timestamptz
);

\\copy standby_last_used (kind, id, last_used_at) FROM '${touch_dump}' WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');

WITH updated_accounts AS (
  UPDATE public.accounts a
  SET last_used_at = GREATEST(COALESCE(a.last_used_at, '-infinity'::timestamptz), s.last_used_at),
      updated_at = GREATEST(a.updated_at, s.last_used_at)
  FROM standby_last_used s
  WHERE s.kind = 'account'
    AND s.id = a.id
    AND s.last_used_at IS NOT NULL
    AND (a.last_used_at IS NULL OR s.last_used_at > a.last_used_at)
  RETURNING a.id
),
updated_api_keys AS (
  UPDATE public.api_keys k
  SET last_used_at = GREATEST(COALESCE(k.last_used_at, '-infinity'::timestamptz), s.last_used_at),
      updated_at = GREATEST(k.updated_at, s.last_used_at)
  FROM standby_last_used s
  WHERE s.kind = 'api_key'
    AND s.id = k.id
    AND s.last_used_at IS NOT NULL
    AND (k.last_used_at IS NULL OR s.last_used_at > k.last_used_at)
  RETURNING k.id
)
SELECT
  'pulled_standby_last_used accounts=' || (SELECT count(*) FROM updated_accounts)
  || ' api_keys=' || (SELECT count(*) FROM updated_api_keys);
SQL
}

exclude_args=()
case "${SYNC_MODE}" in
  full)
    ;;
  core)
    exclude_args+=("--exclude-table-data=public.ops_*")
    exclude_args+=("--exclude-table-data=public.usage_dashboard_hourly")
    exclude_args+=("--exclude-table-data=public.usage_dashboard_hourly_users")
    ;;
  *)
    echo "Unsupported SYNC_MODE=${SYNC_MODE}. Use full or core." >&2
    exit 1
    ;;
esac

case "${STANDBY_RESTORE_STRATEGY}" in
  shadow_swap|in_place)
    ;;
  *)
    echo "Unsupported STANDBY_RESTORE_STRATEGY=${STANDBY_RESTORE_STRATEGY}. Use shadow_swap or in_place." >&2
    exit 1
    ;;
esac

if [[ "${PULL_STANDBY_OPENAI_OAUTH_TOKENS}" == "true" ]]; then
  pull_standby_openai_oauth_tokens
fi

if [[ "${PULL_STANDBY_RUNTIME_RECORDS}" == "true" ]]; then
  pull_standby_runtime_records
fi

if [[ "${LOCAL_PG_SOURCE}" == "docker" ]]; then
  echo "Creating ${SYNC_MODE} PostgreSQL snapshot from local ${LOCAL_PG_CONTAINER} ..."
else
  echo "Creating ${SYNC_MODE} PostgreSQL snapshot from local ${LOCAL_PG_HOST}:${LOCAL_PG_PORT}/${PGDATABASE} ..."
fi
local_pg_dump \
  -Fc \
  --no-owner \
  --no-privileges \
  "${exclude_args[@]}" \
  > "${dump_path}"

dump_size="$(du -h "${dump_path}" | awk '{print $1}')"
echo "Snapshot created: ${dump_size}"

echo "Uploading snapshot to VPS ..."
remote_exec "install -d -m 0755 $(shell_quote "${REMOTE_DIR}/backups")"
remote_copy "${dump_path}" "${remote_dump}"

echo "Restoring snapshot on VPS standby database ..."
pgdatabase_sql="$(sql_literal "${PGDATABASE}")"
ignore_proxy_ids_sql="$(sql_literal "${STANDBY_IGNORE_PROXY_IDS}")"
standby_openai_request_refresh_sql="$(sql_literal "${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED}")"
standby_request_refresh_sql="$(sql_literal "${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED}")"
restore_db="${PGDATABASE}_restore_${timestamp_id}"
previous_db="${PGDATABASE}_previous_${timestamp_id}"

if [[ ! "${PGDATABASE}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  echo "PGDATABASE=${PGDATABASE} is not a safe PostgreSQL identifier for database rename." >&2
  exit 1
fi

if [[ "${STANDBY_RESTORE_STRATEGY}" == "in_place" ]]; then
  remote_exec "set -euo pipefail
systemctl stop $(shell_quote "${SERVICE_NAME}") || true
docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d postgres -v ON_ERROR_STOP=1 -c \"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ${pgdatabase_sql} AND pid <> pg_backend_pid();\"
docker exec sub2api-standby-postgres dropdb -U $(shell_quote "${PGUSER}") --if-exists $(shell_quote "${PGDATABASE}")
docker exec sub2api-standby-postgres createdb -U $(shell_quote "${PGUSER}") -O $(shell_quote "${PGUSER}") $(shell_quote "${PGDATABASE}")
docker exec -i sub2api-standby-postgres pg_restore -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") --no-owner --no-privileges < $(shell_quote "${remote_dump}")
if [[ $(shell_quote "${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'oauth'
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_cleared_openai_oauth_proxy_accounts=' || count(*) FROM updated;\"
elif [[ -n $(shell_quote "${STANDBY_IGNORE_PROXY_IDS}") ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
WITH ignored_proxy_ids AS (
  SELECT DISTINCT trim(value)::bigint AS id
  FROM regexp_split_to_table(${ignore_proxy_ids_sql}, ',') AS value
  WHERE trim(value) ~ '^[0-9]+$'
),
updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE proxy_id IN (SELECT id FROM ignored_proxy_ids)
  RETURNING id
)
SELECT 'standby_ignored_proxy_accounts=' || count(*) FROM updated;\"
fi
if [[ $(shell_quote "${STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'apikey'
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_cleared_openai_apikey_proxy_accounts=' || count(*) FROM updated;\"
fi

if [[ $(shell_quote "${STANDBY_CLEAR_GROK_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'grok'
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_cleared_grok_proxy_accounts=' || count(*) FROM updated;\"
fi
if [[ -n $(shell_quote "${STANDBY_CLEAR_LOCALHOST_PROXY_IDS}") ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
WITH bad_proxy_ids AS (
  SELECT DISTINCT trim(value)::bigint AS id
  FROM regexp_split_to_table($(sql_literal "${STANDBY_CLEAR_LOCALHOST_PROXY_IDS}"), ',') AS value
  WHERE trim(value) ~ '^[0-9]+$'
),
updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND proxy_id IN (SELECT id FROM bad_proxy_ids)
  RETURNING id
)
SELECT 'standby_cleared_localhost_proxy_accounts=' || count(*) FROM updated;\"
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
UPDATE public.proxies
SET status = 'inactive', updated_at = now()
WHERE deleted_at IS NULL
  AND id IN (
    SELECT DISTINCT trim(value)::bigint
    FROM regexp_split_to_table($(sql_literal "${STANDBY_CLEAR_LOCALHOST_PROXY_IDS}"), ',') AS value
    WHERE trim(value) ~ '^[0-9]+$'
  )
  AND status <> 'inactive';
SELECT 'standby_inactivated_localhost_proxies_ok' AS result;\"
fi
docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
DO \\\$\\\$
DECLARE
  remaining_count integer;
  sample text;
BEGIN
  SELECT count(*)
    INTO remaining_count
  FROM public.accounts a
  WHERE a.platform = 'openai'
    AND a.type = 'apikey'
    AND a.deleted_at IS NULL
    AND a.proxy_id IS NOT NULL;

  IF remaining_count > 0 THEN
    SELECT string_agg(
             a.id::text || ':' || a.name || ' proxy_id=' || a.proxy_id::text ||
             COALESCE(' ' || p.protocol || '://' || p.host || ':' || p.port::text, ''),
             ', '
             ORDER BY a.id
           )
      INTO sample
    FROM (
      SELECT id, name, proxy_id
      FROM public.accounts
      WHERE platform = 'openai'
        AND type = 'apikey'
        AND deleted_at IS NULL
        AND proxy_id IS NOT NULL
      ORDER BY id
      LIMIT 10
    ) a
    LEFT JOIN public.proxies p ON p.id = a.proxy_id;

    RAISE EXCEPTION
      'standby snapshot invariant failed: OpenAI API key accounts must not keep proxy_id; remaining=%, sample=%',
      remaining_count, sample;
  END IF;
END \\\$\\\$;

SELECT 'verified_standby_openai_apikey_proxy_ids_cleared=0';\"

# VPS proxy policy after snapshot restore
if [[ $(shell_quote "${STANDBY_PROXY_POLICY}") == "remap_local" ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
INSERT INTO public.proxies (id, name, protocol, host, port, status, created_at, updated_at, fallback_mode, expiry_warn_days)
VALUES (
  ${STANDBY_LOCAL_PROXY_ID},
  $(sql_literal "${STANDBY_LOCAL_PROXY_NAME}"),
  $(sql_literal "${STANDBY_LOCAL_PROXY_PROTOCOL}"),
  $(sql_literal "${STANDBY_LOCAL_PROXY_HOST}"),
  ${STANDBY_LOCAL_PROXY_PORT},
  'active', now(), now(), 'none', 7
)
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name,
  protocol = EXCLUDED.protocol,
  host = EXCLUDED.host,
  port = EXCLUDED.port,
  status = 'active',
  deleted_at = NULL,
  updated_at = now();

UPDATE public.proxies
SET protocol = $(sql_literal "${STANDBY_LOCAL_PROXY_PROTOCOL}"),
    host = $(sql_literal "${STANDBY_LOCAL_PROXY_HOST}"),
    port = ${STANDBY_LOCAL_PROXY_PORT},
    status = 'active',
    deleted_at = NULL,
    updated_at = now()
WHERE deleted_at IS NULL
  AND (host IN ('127.0.0.1', 'localhost') OR id = ${STANDBY_LOCAL_PROXY_ID});

WITH remapped AS (
  UPDATE public.accounts
  SET proxy_id = ${STANDBY_LOCAL_PROXY_ID}, updated_at = now()
  WHERE deleted_at IS NULL
    AND proxy_id IS NOT NULL
    AND proxy_id IS DISTINCT FROM ${STANDBY_LOCAL_PROXY_ID}
  RETURNING id
)
SELECT 'standby_proxy_policy=remap_local local_proxy_id=' || ${STANDBY_LOCAL_PROXY_ID}::text ||
       ' remapped=' || (SELECT count(*)::text FROM remapped) ||
       ' accounts_with_proxy=' || (
         SELECT count(*)::text FROM public.accounts
         WHERE deleted_at IS NULL AND proxy_id IS NOT NULL
       );\"
elif [[ $(shell_quote "${STANDBY_PROXY_POLICY}") == "clear_all" || $(shell_quote "${STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_proxy_policy=clear_all cleared=' || count(*) FROM updated;\"
fi
if [[ -n $(shell_quote "${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED}") ]]; then
  if grep -q '^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
    sed -i \"s/^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED}/\" /etc/sub2api-standby.env
  else
    printf '\\nTOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=%s\\n' ${standby_openai_request_refresh_sql} >> /etc/sub2api-standby.env
  fi
fi
if [[ -n $(shell_quote "${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED}") ]]; then
  if grep -q '^TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
    sed -i \"s/^TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED}/\" /etc/sub2api-standby.env
  else
    printf '\\nTOKEN_REFRESH_REQUEST_REFRESH_ENABLED=%s\\n' ${standby_request_refresh_sql} >> /etc/sub2api-standby.env
  fi
fi
if [[ $(shell_quote "${START_STANDBY_AFTER_SYNC}") == true ]]; then
  systemctl start $(shell_quote "${SERVICE_NAME}")
  for i in \$(seq 1 60); do
    if curl -fsS --max-time 3 http://$(shell_quote "${VPS_TAILSCALE_IP}"):${STANDBY_SERVER_PORT}/health >/dev/null 2>&1; then
      exit 0
    fi
    sleep 1
  done
  systemctl --no-pager --full status $(shell_quote "${SERVICE_NAME}") || true
  exit 1
else
  systemctl reset-failed $(shell_quote "${SERVICE_NAME}") || true
  if ss -ltnp | grep -q ':${STANDBY_SERVER_PORT}'; then
    echo 'standby service still appears to be listening after stop' >&2
    exit 1
  fi
fi"
else
  remote_exec "set -euo pipefail
active_db=$(shell_quote "${PGDATABASE}")
restore_db=$(shell_quote "${restore_db}")
previous_db=$(shell_quote "${previous_db}")
keep_previous=$(shell_quote "${STANDBY_KEEP_PREVIOUS_DBS}")
runtime_path=$(shell_quote "${standby_anthropic_runtime_path}")

if [[ ! \"\${active_db}\" =~ ^[A-Za-z_][A-Za-z0-9_]*$ || ! \"\${restore_db}\" =~ ^[A-Za-z_][A-Za-z0-9_]*$ || ! \"\${previous_db}\" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  echo 'unsafe database identifier for shadow swap' >&2
  exit 1
fi

cleanup_runtime_snapshot() {
  rm -f \"\${runtime_path}\"
  docker exec sub2api-standby-postgres rm -f \"\${runtime_path}\" >/dev/null 2>&1 || true
}
trap cleanup_runtime_snapshot EXIT

echo \"Preparing shadow database \${restore_db} while \${active_db} keeps serving ...\"
docker exec sub2api-standby-postgres dropdb -U $(shell_quote "${PGUSER}") --if-exists \"\${restore_db}\"
docker exec sub2api-standby-postgres createdb -U $(shell_quote "${PGUSER}") -O $(shell_quote "${PGUSER}") \"\${restore_db}\"
docker exec -i sub2api-standby-postgres pg_restore -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" --no-owner --no-privileges < $(shell_quote "${remote_dump}")
if [[ $(shell_quote "${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'oauth'
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_cleared_openai_oauth_proxy_accounts=' || count(*) FROM updated;\"
elif [[ -n $(shell_quote "${STANDBY_IGNORE_PROXY_IDS}") ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
WITH ignored_proxy_ids AS (
  SELECT DISTINCT trim(value)::bigint AS id
  FROM regexp_split_to_table(${ignore_proxy_ids_sql}, ',') AS value
  WHERE trim(value) ~ '^[0-9]+$'
),
updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE proxy_id IN (SELECT id FROM ignored_proxy_ids)
  RETURNING id
)
SELECT 'standby_ignored_proxy_accounts=' || count(*) FROM updated;\"
fi
if [[ $(shell_quote "${STANDBY_CLEAR_OPENAI_APIKEY_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'apikey'
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_cleared_openai_apikey_proxy_accounts=' || count(*) FROM updated;\"
fi

if [[ $(shell_quote "${STANDBY_CLEAR_GROK_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'grok'
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_cleared_grok_proxy_accounts=' || count(*) FROM updated;\"
fi
if [[ -n $(shell_quote "${STANDBY_CLEAR_LOCALHOST_PROXY_IDS}") ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
WITH bad_proxy_ids AS (
  SELECT DISTINCT trim(value)::bigint AS id
  FROM regexp_split_to_table($(sql_literal "${STANDBY_CLEAR_LOCALHOST_PROXY_IDS}"), ',') AS value
  WHERE trim(value) ~ '^[0-9]+$'
),
updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND proxy_id IN (SELECT id FROM bad_proxy_ids)
  RETURNING id
)
SELECT 'standby_cleared_localhost_proxy_accounts=' || count(*) FROM updated;\"
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
UPDATE public.proxies
SET status = 'inactive', updated_at = now()
WHERE deleted_at IS NULL
  AND id IN (
    SELECT DISTINCT trim(value)::bigint
    FROM regexp_split_to_table($(sql_literal "${STANDBY_CLEAR_LOCALHOST_PROXY_IDS}"), ',') AS value
    WHERE trim(value) ~ '^[0-9]+$'
  )
  AND status <> 'inactive';
SELECT 'standby_inactivated_localhost_proxies_ok' AS result;\"
fi
docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
DO \\\$\\\$
DECLARE
  remaining_count integer;
  sample text;
BEGIN
  SELECT count(*)
    INTO remaining_count
  FROM public.accounts a
  WHERE a.platform = 'openai'
    AND a.type = 'apikey'
    AND a.deleted_at IS NULL
    AND a.proxy_id IS NOT NULL;

  IF remaining_count > 0 THEN
    SELECT string_agg(
             a.id::text || ':' || a.name || ' proxy_id=' || a.proxy_id::text ||
             COALESCE(' ' || p.protocol || '://' || p.host || ':' || p.port::text, ''),
             ', '
             ORDER BY a.id
           )
      INTO sample
    FROM (
      SELECT id, name, proxy_id
      FROM public.accounts
      WHERE platform = 'openai'
        AND type = 'apikey'
        AND deleted_at IS NULL
        AND proxy_id IS NOT NULL
      ORDER BY id
      LIMIT 10
    ) a
    LEFT JOIN public.proxies p ON p.id = a.proxy_id;

    RAISE EXCEPTION
      'standby snapshot invariant failed: OpenAI API key accounts must not keep proxy_id; remaining=%, sample=%',
      remaining_count, sample;
  END IF;
END \\\$\\\$;

SELECT 'verified_standby_openai_apikey_proxy_ids_cleared=0';\"

# VPS proxy policy after snapshot restore
if [[ $(shell_quote "${STANDBY_PROXY_POLICY}") == "remap_local" ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
INSERT INTO public.proxies (id, name, protocol, host, port, status, created_at, updated_at, fallback_mode, expiry_warn_days)
VALUES (
  ${STANDBY_LOCAL_PROXY_ID},
  $(sql_literal "${STANDBY_LOCAL_PROXY_NAME}"),
  $(sql_literal "${STANDBY_LOCAL_PROXY_PROTOCOL}"),
  $(sql_literal "${STANDBY_LOCAL_PROXY_HOST}"),
  ${STANDBY_LOCAL_PROXY_PORT},
  'active', now(), now(), 'none', 7
)
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name,
  protocol = EXCLUDED.protocol,
  host = EXCLUDED.host,
  port = EXCLUDED.port,
  status = 'active',
  deleted_at = NULL,
  updated_at = now();

UPDATE public.proxies
SET protocol = $(sql_literal "${STANDBY_LOCAL_PROXY_PROTOCOL}"),
    host = $(sql_literal "${STANDBY_LOCAL_PROXY_HOST}"),
    port = ${STANDBY_LOCAL_PROXY_PORT},
    status = 'active',
    deleted_at = NULL,
    updated_at = now()
WHERE deleted_at IS NULL
  AND (host IN ('127.0.0.1', 'localhost') OR id = ${STANDBY_LOCAL_PROXY_ID});

WITH remapped AS (
  UPDATE public.accounts
  SET proxy_id = ${STANDBY_LOCAL_PROXY_ID}, updated_at = now()
  WHERE deleted_at IS NULL
    AND proxy_id IS NOT NULL
    AND proxy_id IS DISTINCT FROM ${STANDBY_LOCAL_PROXY_ID}
  RETURNING id
)
SELECT 'standby_proxy_policy=remap_local local_proxy_id=' || ${STANDBY_LOCAL_PROXY_ID}::text ||
       ' remapped=' || (SELECT count(*)::text FROM remapped) ||
       ' accounts_with_proxy=' || (
         SELECT count(*)::text FROM public.accounts
         WHERE deleted_at IS NULL AND proxy_id IS NOT NULL
       );\"
elif [[ $(shell_quote "${STANDBY_PROXY_POLICY}") == "clear_all" || $(shell_quote "${STANDBY_CLEAR_ALL_ACCOUNT_PROXY_IDS}") == true ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 -c \"
WITH updated AS (
  UPDATE public.accounts
  SET proxy_id = NULL, updated_at = now()
  WHERE deleted_at IS NULL
    AND proxy_id IS NOT NULL
  RETURNING id
)
SELECT 'standby_proxy_policy=clear_all cleared=' || count(*) FROM updated;\"
fi
if [[ -n $(shell_quote "${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED}") ]]; then
  if grep -q '^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
    sed -i \"s/^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=${STANDBY_TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED}/\" /etc/sub2api-standby.env
  else
    printf '\\nTOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=%s\\n' ${standby_openai_request_refresh_sql} >> /etc/sub2api-standby.env
  fi
fi
if [[ -n $(shell_quote "${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED}") ]]; then
  if grep -q '^TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
    sed -i \"s/^TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=${STANDBY_TOKEN_REFRESH_REQUEST_REFRESH_ENABLED}/\" /etc/sub2api-standby.env
  else
    printf '\\nTOKEN_REFRESH_REQUEST_REFRESH_ENABLED=%s\\n' ${standby_request_refresh_sql} >> /etc/sub2api-standby.env
  fi
fi
if [[ $(shell_quote "${START_STANDBY_AFTER_SYNC}") == true ]]; then
  echo \"Swapping \${restore_db} into \${active_db}; service downtime starts now ...\"
  swap_start=\$(date +%s)
  systemctl stop $(shell_quote "${SERVICE_NAME}") || true
  # The VPS is the source of truth for runtime state generated by live Anthropic traffic.
  # Capture it only after the service stops so no request can update the old active DB after
  # this point. The local dump remains the source of credentials and configuration.
  echo \"Preserving Anthropic setup-token runtime state from \${active_db} ...\"
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${active_db}\" -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    a.id,
    a.session_window_start,
    a.session_window_end,
    a.session_window_status,
    a.rate_limited_at,
    a.rate_limit_reset_at,
    a.overload_until,
    a.temp_unschedulable_until,
    a.temp_unschedulable_reason,
    a.updated_at,
    jsonb_build_object(
      'model_rate_limits', a.extra->'model_rate_limits',
      'session_window_utilization', a.extra->'session_window_utilization',
      'passive_usage_7d_utilization', a.extra->'passive_usage_7d_utilization',
      'passive_usage_7d_reset', a.extra->'passive_usage_7d_reset',
      'passive_usage_7d_oi_utilization', a.extra->'passive_usage_7d_oi_utilization',
      'passive_usage_7d_oi_reset', a.extra->'passive_usage_7d_oi_reset',
      'passive_usage_sampled_at', a.extra->'passive_usage_sampled_at'
    ) AS runtime_extra
  FROM public.accounts a
  WHERE a.deleted_at IS NULL
    AND a.platform = 'anthropic'
    AND a.type = 'setup-token'
  ORDER BY a.id
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\\\t');
\" > \"\${runtime_path}\"
  docker cp \"\${runtime_path}\" \"sub2api-standby-postgres:\${runtime_path}\"
  docker exec -i sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d \"\${restore_db}\" -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE standby_anthropic_setup_runtime (
  id bigint PRIMARY KEY,
  session_window_start timestamptz,
  session_window_end timestamptz,
  session_window_status varchar(20),
  rate_limited_at timestamptz,
  rate_limit_reset_at timestamptz,
  overload_until timestamptz,
  temp_unschedulable_until timestamptz,
  temp_unschedulable_reason text,
  updated_at timestamptz NOT NULL,
  runtime_extra jsonb NOT NULL
);
\\copy standby_anthropic_setup_runtime FROM '\${runtime_path}' WITH (FORMAT csv, HEADER true, DELIMITER E'\\\\t');

WITH updated AS (
  UPDATE public.accounts a
  SET session_window_start = r.session_window_start,
      session_window_end = r.session_window_end,
      session_window_status = r.session_window_status,
      rate_limited_at = r.rate_limited_at,
      rate_limit_reset_at = r.rate_limit_reset_at,
      overload_until = r.overload_until,
      temp_unschedulable_until = r.temp_unschedulable_until,
      temp_unschedulable_reason = r.temp_unschedulable_reason,
      extra = (
        COALESCE(a.extra, '{}'::jsonb) - ARRAY[
          'model_rate_limits',
          'session_window_utilization',
          'passive_usage_7d_utilization',
          'passive_usage_7d_reset',
          'passive_usage_7d_oi_utilization',
          'passive_usage_7d_oi_reset',
          'passive_usage_sampled_at'
        ]
      ) || r.runtime_extra,
      updated_at = GREATEST(a.updated_at, r.updated_at)
  FROM standby_anthropic_setup_runtime r
  WHERE a.id = r.id
    AND a.deleted_at IS NULL
    AND a.platform = 'anthropic'
    AND a.type = 'setup-token'
  RETURNING a.id
)
SELECT 'preserved_vps_anthropic_setup_runtime=' || count(*) FROM updated;
SQL
  rm -f \"\${runtime_path}\"
  docker exec sub2api-standby-postgres rm -f \"\${runtime_path}\"
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d postgres -v ON_ERROR_STOP=1 -c \"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('\${active_db}', '\${restore_db}') AND pid <> pg_backend_pid();\"
  docker exec sub2api-standby-postgres dropdb -U $(shell_quote "${PGUSER}") --if-exists \"\${previous_db}\"
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d postgres -v ON_ERROR_STOP=1 -c \"ALTER DATABASE \\\"\${active_db}\\\" RENAME TO \\\"\${previous_db}\\\";\"
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d postgres -v ON_ERROR_STOP=1 -c \"ALTER DATABASE \\\"\${restore_db}\\\" RENAME TO \\\"\${active_db}\\\";\"
  systemctl start $(shell_quote "${SERVICE_NAME}")
  for i in \$(seq 1 60); do
    if curl -fsS --max-time 3 http://$(shell_quote "${VPS_TAILSCALE_IP}"):${STANDBY_SERVER_PORT}/health >/dev/null 2>&1; then
      swap_end=\$(date +%s)
      echo \"shadow_swap_downtime_seconds=\$((swap_end - swap_start))\"
      break
    fi
    sleep 1
  done
  if ! curl -fsS --max-time 3 http://$(shell_quote "${VPS_TAILSCALE_IP}"):${STANDBY_SERVER_PORT}/health >/dev/null 2>&1; then
    systemctl --no-pager --full status $(shell_quote "${SERVICE_NAME}") || true
    exit 1
  fi
else
  echo \"Shadow database \${restore_db} restored; START_STANDBY_AFTER_SYNC=false so active database was not swapped.\"
  systemctl reset-failed $(shell_quote "${SERVICE_NAME}") || true
fi

if [[ \"\${keep_previous}\" =~ ^[0-9]+$ ]]; then
  mapfile -t old_previous_dbs < <(docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d postgres -Atc \"SELECT datname FROM pg_database WHERE datname LIKE '\${active_db}_previous_%' ORDER BY datname DESC OFFSET \${keep_previous};\")
  for old_db in \"\${old_previous_dbs[@]}\"; do
    if [[ \"\${old_db}\" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
      docker exec sub2api-standby-postgres dropdb -U $(shell_quote "${PGUSER}") --if-exists \"\${old_db}\" || true
    fi
  done
fi"
fi

echo "Collecting remote verification ..."
remote_exec "set -euo pipefail
docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -Atc \"
select 'tables=' || count(*) from information_schema.tables where table_schema='public';
select 'db_size=' || pg_size_pretty(pg_database_size(current_database()));
select 'users=' || case when to_regclass('public.users') is null then 0 else (select count(*) from public.users) end;
select 'accounts=' || case when to_regclass('public.accounts') is null then 0 else (select count(*) from public.accounts) end;
select 'api_keys=' || case when to_regclass('public.api_keys') is null then 0 else (select count(*) from public.api_keys) end;
\"
if [[ $(shell_quote "${START_STANDBY_AFTER_SYNC}") == true ]]; then
  curl -fsS --max-time 5 http://$(shell_quote "${VPS_TAILSCALE_IP}"):${STANDBY_SERVER_PORT}/health
  echo
else
  systemctl is-active $(shell_quote "${SERVICE_NAME}") || true
fi"

echo "Warm standby sync completed: ${remote_dump}"
