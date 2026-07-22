#!/usr/bin/env bash
set -euo pipefail

# Pull VPS usage logs and Anthropic Setup Token runtime state into local DB (merge, no delete).

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-15}"
SSH_RETRIES="${SSH_RETRIES:-4}"
SSH_RETRY_SLEEP_SECONDS="${SSH_RETRY_SLEEP_SECONDS:-8}"
REMOTE_QUERY_TIMEOUT_SECONDS="${REMOTE_QUERY_TIMEOUT_SECONDS:-90}"
LOCAL_PG_CONTAINER="${LOCAL_PG_CONTAINER:-sub2api-postgres}"
LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-127.0.0.1}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-5432}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${PGPASSWORD:-}}"
PGUSER="${PGUSER:-sub2api}"
PGDATABASE="${PGDATABASE:-sub2api}"
USAGE_PULL_LOOKBACK_HOURS="${USAGE_PULL_LOOKBACK_HOURS:-48}"

tmp_dir="$(mktemp -d)"
usage_path="${tmp_dir}/vps-usage-logs.tsv"
error_path="${tmp_dir}/vps-ops-error-logs.tsv"
touch_path="${tmp_dir}/vps-last-used.tsv"
runtime_path="${tmp_dir}/vps-anthropic-setup-runtime.tsv"

cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

local_psql() {
  case "${LOCAL_PG_SOURCE}" in
    host)
      if [[ -z "${LOCAL_PG_PASSWORD}" ]]; then
        echo "LOCAL_PG_PASSWORD or PGPASSWORD is required when LOCAL_PG_SOURCE=host" >&2
        exit 1
      fi
      PGPASSWORD="${LOCAL_PG_PASSWORD}" psql -h "${LOCAL_PG_HOST}" -p "${LOCAL_PG_PORT}" -U "${PGUSER}" -d "${PGDATABASE}" "$@"
      ;;
    docker)
      docker exec -i "${LOCAL_PG_CONTAINER}" psql -U "${PGUSER}" -d "${PGDATABASE}" "$@"
      ;;
    *)
      echo "Unsupported LOCAL_PG_SOURCE=${LOCAL_PG_SOURCE}. Use host or docker." >&2
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


echo "Pulling VPS usage records (lookback=${USAGE_PULL_LOOKBACK_HOURS}h) ..."

remote_exec "timeout $(printf "%q" "${REMOTE_QUERY_TIMEOUT_SECONDS}") docker exec sub2api-standby-postgres psql -U $(printf "%q" "${PGUSER}") -d $(printf "%q" "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
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
  WHERE created_at >= now() - make_interval(hours => ${USAGE_PULL_LOOKBACK_HOURS})
  ORDER BY created_at, id
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${usage_path}"

remote_exec "timeout $(printf "%q" "${REMOTE_QUERY_TIMEOUT_SECONDS}") docker exec sub2api-standby-postgres psql -U $(printf "%q" "${PGUSER}") -d $(printf "%q" "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    request_id, client_request_id, user_id, api_key_id, account_id, group_id,
    host(client_ip)::text AS client_ip, platform, model, request_path, stream, user_agent,
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
  WHERE created_at >= now() - make_interval(hours => ${USAGE_PULL_LOOKBACK_HOURS})
  ORDER BY created_at, id
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${error_path}"

remote_exec "timeout $(printf "%q" "${REMOTE_QUERY_TIMEOUT_SECONDS}") docker exec sub2api-standby-postgres psql -U $(printf "%q" "${PGUSER}") -d $(printf "%q" "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT 'account'::text AS kind, id, last_used_at
  FROM public.accounts
  WHERE deleted_at IS NULL AND last_used_at IS NOT NULL
  UNION ALL
  SELECT 'api_key'::text AS kind, id, last_used_at
  FROM public.api_keys
  WHERE deleted_at IS NULL AND last_used_at IS NOT NULL
  ORDER BY 1, 2
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${touch_path}"

# The VPS is the runtime source of truth for setup-token accounts because it serves the traffic.
# Include JSON nulls for absent keys so old local passive samples are cleared instead of surviving
# indefinitely after the remote state has reset.
remote_exec "timeout $(printf "%q" "${REMOTE_QUERY_TIMEOUT_SECONDS}") docker exec sub2api-standby-postgres psql -U $(printf "%q" "${PGUSER}") -d $(printf "%q" "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
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
) TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER E'\\t');
\"" > "${runtime_path}"

usage_rows=$(($(wc -l < "${usage_path}" | tr -d ' ') - 1)); [[ $usage_rows -lt 0 ]] && usage_rows=0
error_rows=$(($(wc -l < "${error_path}" | tr -d ' ') - 1)); [[ $error_rows -lt 0 ]] && error_rows=0
touch_rows=$(($(wc -l < "${touch_path}" | tr -d ' ') - 1)); [[ $touch_rows -lt 0 ]] && touch_rows=0
runtime_rows=$(($(wc -l < "${runtime_path}" | tr -d ' ') - 1)); [[ $runtime_rows -lt 0 ]] && runtime_rows=0
echo "Pulled usage_rows=${usage_rows} ops_error_rows=${error_rows} last_used_rows=${touch_rows} anthropic_setup_runtime_rows=${runtime_rows}"

local_psql -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE vps_usage_logs (
  user_id bigint, api_key_id bigint, account_id bigint, request_id varchar(64), model varchar(100),
  input_tokens int, output_tokens int, cache_creation_tokens int, cache_read_tokens int,
  cache_creation_5m_tokens int, cache_creation_1h_tokens int,
  input_cost numeric, output_cost numeric, cache_creation_cost numeric, cache_read_cost numeric,
  total_cost numeric, actual_cost numeric, stream boolean, duration_ms int, created_at timestamptz,
  group_id bigint, subscription_id bigint, rate_multiplier numeric, first_token_ms int,
  billing_type smallint, user_agent varchar(512), image_count int, image_size varchar(10), ip_address varchar(45),
  account_rate_multiplier numeric, reasoning_effort varchar(20), cache_ttl_overridden boolean,
  openai_ws_mode boolean, request_type smallint, service_tier varchar(16), inbound_endpoint varchar(128),
  upstream_endpoint varchar(128), upstream_model varchar(100), requested_model varchar(100), channel_id bigint,
  model_mapping_chain varchar(500), billing_tier varchar(50), billing_mode varchar(20),
  image_output_tokens int, image_output_cost numeric, ingress_node varchar(128), account_stats_cost numeric
);
\copy vps_usage_logs FROM '${usage_path}' WITH (FORMAT csv, HEADER true, DELIMITER E'\t');

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
    COALESCE(t.input_tokens,0), COALESCE(t.output_tokens,0), COALESCE(t.cache_creation_tokens,0), COALESCE(t.cache_read_tokens,0),
    COALESCE(t.cache_creation_5m_tokens,0), COALESCE(t.cache_creation_1h_tokens,0),
    COALESCE(t.input_cost,0), COALESCE(t.output_cost,0), COALESCE(t.cache_creation_cost,0), COALESCE(t.cache_read_cost,0),
    COALESCE(t.total_cost,0), COALESCE(t.actual_cost,0), COALESCE(t.stream,false), t.duration_ms, t.created_at,
    t.group_id, t.subscription_id, COALESCE(t.rate_multiplier,1), t.first_token_ms,
    COALESCE(t.billing_type,0), t.user_agent, COALESCE(t.image_count,0), t.image_size, t.ip_address,
    t.account_rate_multiplier, t.reasoning_effort, COALESCE(t.cache_ttl_overridden,false),
    COALESCE(t.openai_ws_mode,false), COALESCE(t.request_type,0), t.service_tier, t.inbound_endpoint,
    t.upstream_endpoint, t.upstream_model, t.requested_model, t.channel_id,
    t.model_mapping_chain, t.billing_tier, t.billing_mode,
    COALESCE(t.image_output_tokens,0), COALESCE(t.image_output_cost,0), t.ingress_node, t.account_stats_cost
  FROM vps_usage_logs t
  WHERE t.created_at IS NOT NULL
    AND t.api_key_id IS NOT NULL
    AND t.account_id IS NOT NULL
  ON CONFLICT (request_id, api_key_id) DO NOTHING
  RETURNING id
)
SELECT 'merged_vps_usage_logs=' || count(*) FROM inserted;

CREATE TEMP TABLE vps_ops_error_logs (
  request_id text, client_request_id text, user_id bigint, api_key_id bigint, account_id bigint, group_id bigint,
  client_ip text, platform text, model text, request_path text, stream boolean, user_agent text,
  error_phase text, error_type text, severity text, status_code int, is_business_limited boolean,
  error_message text, error_body text, error_source text, error_owner text, account_status text,
  upstream_status_code int, upstream_error_message text, upstream_error_detail text,
  provider_error_code text, provider_error_type text, network_error_type text,
  retry_after_seconds int, duration_ms int, time_to_first_token_ms bigint,
  auth_latency_ms bigint, routing_latency_ms bigint, upstream_latency_ms bigint,
  response_latency_ms bigint, created_at timestamptz, upstream_errors jsonb, is_count_tokens boolean,
  resolved boolean, resolved_at timestamptz, resolved_by_user_id bigint,
  inbound_endpoint text, upstream_endpoint text, requested_model text, upstream_model text, request_type smallint,
  attempted_key_prefix text, deleted_key_owner_user_id bigint, deleted_key_name text, api_key_prefix text
);
\copy vps_ops_error_logs FROM '${error_path}' WITH (FORMAT csv, HEADER true, DELIMITER E'\t');

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
    NULLIF(t.client_ip, '')::inet, t.platform, t.model, t.request_path, COALESCE(t.stream,false), t.user_agent,
    t.error_phase, t.error_type, t.severity, t.status_code, COALESCE(t.is_business_limited,false),
    t.error_message, t.error_body, t.error_source, t.error_owner, t.account_status,
    t.upstream_status_code, t.upstream_error_message, t.upstream_error_detail,
    t.provider_error_code, t.provider_error_type, t.network_error_type,
    t.retry_after_seconds, t.duration_ms, t.time_to_first_token_ms,
    t.auth_latency_ms, t.routing_latency_ms, t.upstream_latency_ms,
    t.response_latency_ms, t.created_at, t.upstream_errors, COALESCE(t.is_count_tokens,false),
    COALESCE(t.resolved,false), t.resolved_at, t.resolved_by_user_id, t.inbound_endpoint, t.upstream_endpoint,
    t.requested_model, t.upstream_model, COALESCE(t.request_type,0),
    t.attempted_key_prefix, t.deleted_key_owner_user_id, t.deleted_key_name, t.api_key_prefix
  FROM vps_ops_error_logs t
  WHERE t.created_at IS NOT NULL
    AND NOT EXISTS (
      SELECT 1 FROM public.ops_error_logs e
      WHERE e.created_at = t.created_at
        AND e.request_id IS NOT DISTINCT FROM t.request_id
        AND e.api_key_id IS NOT DISTINCT FROM t.api_key_id
        AND e.account_id IS NOT DISTINCT FROM t.account_id
        AND e.error_phase IS NOT DISTINCT FROM t.error_phase
        AND e.error_type IS NOT DISTINCT FROM t.error_type
    )
  RETURNING id
)
SELECT 'merged_vps_ops_error_logs=' || count(*) FROM inserted;

CREATE TEMP TABLE vps_last_used (
  kind text NOT NULL,
  id bigint NOT NULL,
  last_used_at timestamptz
);
\copy vps_last_used (kind, id, last_used_at) FROM '${touch_path}' WITH (FORMAT csv, HEADER true, DELIMITER E'\t');

WITH ua AS (
  UPDATE public.accounts a
  SET last_used_at = GREATEST(COALESCE(a.last_used_at, '-infinity'::timestamptz), s.last_used_at),
      updated_at = GREATEST(a.updated_at, s.last_used_at)
  FROM vps_last_used s
  WHERE s.kind = 'account' AND s.id = a.id AND s.last_used_at IS NOT NULL
    AND (a.last_used_at IS NULL OR s.last_used_at > a.last_used_at)
  RETURNING a.id
), uk AS (
  UPDATE public.api_keys k
  SET last_used_at = GREATEST(COALESCE(k.last_used_at, '-infinity'::timestamptz), s.last_used_at),
      updated_at = GREATEST(k.updated_at, s.last_used_at)
  FROM vps_last_used s
  WHERE s.kind = 'api_key' AND s.id = k.id AND s.last_used_at IS NOT NULL
    AND (k.last_used_at IS NULL OR s.last_used_at > k.last_used_at)
  RETURNING k.id
)
SELECT 'merged_last_used accounts=' || (SELECT count(*) FROM ua) || ' api_keys=' || (SELECT count(*) FROM uk);

CREATE TEMP TABLE vps_anthropic_setup_runtime (
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
\copy vps_anthropic_setup_runtime FROM '${runtime_path}' WITH (FORMAT csv, HEADER true, DELIMITER E'\t');

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
  FROM vps_anthropic_setup_runtime r
  WHERE a.id = r.id
    AND a.deleted_at IS NULL
    AND a.platform = 'anthropic'
    AND a.type = 'setup-token'
  RETURNING a.id
)
SELECT 'merged_vps_anthropic_setup_runtime=' || count(*) FROM updated;
SQL

echo "VPS usage pull completed."
