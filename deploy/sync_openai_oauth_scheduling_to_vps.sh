#!/usr/bin/env bash
set -euo pipefail

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
REMOTE_COPY_HOST="${REMOTE_COPY_HOST:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-30}"
REMOTE_RETRIES="${REMOTE_RETRIES:-5}"
REMOTE_COMMAND_TIMEOUT="${REMOTE_COMMAND_TIMEOUT:-120}"
LOCAL_PG_CONTAINER="${LOCAL_PG_CONTAINER:-sub2api-postgres}"
LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-127.0.0.1}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-5432}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${PGPASSWORD:-}}"
PGUSER="${PGUSER:-sub2api}"
PGDATABASE="${PGDATABASE:-sub2api}"
STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-6}"
STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-false}"
STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
HOT_GROUP_NAME="${HOT_GROUP_NAME:-codex-warp-verified}"
STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME="${STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME:-}"
STANDBY_REMAP_API_KEYS_TO_GROUP_NAME="${STANDBY_REMAP_API_KEYS_TO_GROUP_NAME:-}"
STANDBY_RESTART_AFTER_SCHEDULING_SYNC="${STANDBY_RESTART_AFTER_SCHEDULING_SYNC:-false}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"

tmp_dir="$(mktemp -d)"
accounts_path="${tmp_dir}/openai-oauth-accounts-scheduling.tsv"
group_defs_path="${tmp_dir}/openai-oauth-groups.tsv"
groups_path="${tmp_dir}/openai-oauth-account-groups.tsv"
users_path="${tmp_dir}/api-key-users.tsv"
remote_accounts_path="/tmp/sub2api-openai-oauth-accounts-scheduling.tsv"
remote_group_defs_path="/tmp/sub2api-openai-oauth-groups.tsv"
remote_groups_path="/tmp/sub2api-openai-oauth-account-groups.tsv"
remote_users_path="/tmp/sub2api-api-key-users.tsv"

cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

shell_quote() {
  printf "%q" "$1"
}

sql_escape() {
  printf "%s" "$1" | sed "s/'/''/g"
}

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
      docker exec "${LOCAL_PG_CONTAINER}" psql -U "${PGUSER}" -d "${PGDATABASE}" "$@"
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
  -o ChannelTimeout=global=120
  -o StrictHostKeyChecking=accept-new
)
if [[ -n "${SSH_PORT}" ]]; then
  ssh_args+=(-p "${SSH_PORT}")
fi
if [[ -n "${SSH_IDENTITY_FILE}" ]]; then
  ssh_args+=(-i "${SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes)
fi

retry_remote() {
  local label="$1"
  shift
  local attempt=1
  local delay=2
  local rc=0
  while (( attempt <= REMOTE_RETRIES )); do
    set +e
    "$@"
    rc=$?
    set -e
    if (( rc == 0 )); then
      return 0
    fi
    if (( attempt == REMOTE_RETRIES )); then
      echo "${label} failed after ${attempt} attempts, exit=${rc}" >&2
      return "${rc}"
    fi
    echo "${label} failed on attempt ${attempt}/${REMOTE_RETRIES}, exit=${rc}; retrying in ${delay}s ..." >&2
    sleep "${delay}"
    attempt=$((attempt + 1))
    delay=$((delay * 2))
  done
}

run_with_timeout() {
  local timeout_seconds="$1"
  shift
  "$@" &
  local command_pid=$!
  (
    sleep "${timeout_seconds}"
    kill -TERM "${command_pid}" 2>/dev/null || true
    sleep 2
    kill -KILL "${command_pid}" 2>/dev/null || true
  ) &
  local watchdog_pid=$!
  set +e
  wait "${command_pid}"
  local rc=$?
  set -e
  kill "${watchdog_pid}" 2>/dev/null || true
  wait "${watchdog_pid}" 2>/dev/null || true
  return "${rc}"
}

remote_exec_once() {
  run_with_timeout "${REMOTE_COMMAND_TIMEOUT}" ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" "$@"
}

remote_exec() {
  retry_remote "remote_exec" remote_exec_once "$@"
}

remote_copy_once() {
  gzip -c "$1" | run_with_timeout "${REMOTE_COMMAND_TIMEOUT}" ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "gzip -dc > $(shell_quote "$2") && test -f $(shell_quote "$2")"
}

remote_copy() {
  retry_remote "remote_copy $1 -> $2" remote_copy_once "$1" "$2"
}

STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME_SQL="$(sql_escape "${STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME}")"
STANDBY_REMAP_API_KEYS_TO_GROUP_NAME_SQL="$(sql_escape "${STANDBY_REMAP_API_KEYS_TO_GROUP_NAME}")"
STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS="${STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS:-true}"

echo "Exporting local OpenAI OAuth scheduling fields ..."
echo "Standby permanent error sync: ${STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS}"
local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    id,
    status,
    schedulable,
    error_message,
    deleted_at,
    rate_limited_at,
    rate_limit_reset_at,
    overload_until,
    session_window_status,
    temp_unschedulable_until,
    temp_unschedulable_reason,
    rate_multiplier,
    proxy_id,
    updated_at
  FROM public.accounts
  WHERE platform = 'openai'
    AND type = 'oauth'
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${accounts_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT DISTINCT
    g.name,
    g.description,
    g.rate_multiplier,
    g.is_exclusive,
    g.status,
    g.platform,
    g.subscription_type,
    g.daily_limit_usd,
    g.weekly_limit_usd,
    g.monthly_limit_usd,
    g.default_validity_days,
    g.image_price_1k,
    g.image_price_2k,
    g.image_price_4k,
    g.claude_code_only,
    g.model_routing,
    g.model_routing_enabled,
    g.mcp_xml_inject,
    g.supported_model_scopes,
    g.sort_order,
    g.allow_messages_dispatch,
    g.default_mapped_model,
    g.require_oauth_only,
    g.require_privacy_set,
    g.messages_dispatch_model_config,
    g.hybrid_dispatch,
    g.hybrid_dispatch_enabled,
    g.rpm_limit,
    g.allow_image_generation,
    g.image_rate_independent,
    g.image_rate_multiplier,
    g.models_list_config,
    g.updated_at
  FROM public.groups g
  JOIN public.account_groups ag ON ag.group_id = g.id
  JOIN public.accounts a ON a.id = ag.account_id
  WHERE g.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${group_defs_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    ag.account_id,
    g.name AS group_name,
    ag.priority,
    ag.created_at
  FROM public.account_groups ag
  JOIN public.accounts a ON a.id = ag.account_id
  JOIN public.groups g ON g.id = ag.group_id
  WHERE a.platform = 'openai'
    AND a.type = 'oauth'
    AND g.deleted_at IS NULL
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${groups_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT DISTINCT
    u.id,
    u.role,
    u.balance,
    u.concurrency,
    u.status,
    u.rpm_limit,
    u.updated_at
  FROM public.users u
  JOIN public.api_keys k ON k.user_id = u.id
  WHERE u.deleted_at IS NULL
    AND k.deleted_at IS NULL
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${users_path}"

accounts_count="$(wc -l < "${accounts_path}" | tr -d ' ')"
group_defs_count="$(wc -l < "${group_defs_path}" | tr -d ' ')"
groups_count="$(wc -l < "${groups_path}" | tr -d ' ')"
users_count="$(wc -l < "${users_path}" | tr -d ' ')"
echo "Exported accounts=${accounts_count} groups=${group_defs_count} account_groups=${groups_count} api_key_users=${users_count}"

if [[ "${accounts_count}" == "0" ]]; then
  exit 0
fi

echo "Uploading OpenAI OAuth scheduling fields to VPS ..."
remote_copy "${accounts_path}" "${remote_accounts_path}"
remote_copy "${group_defs_path}" "${remote_group_defs_path}"
remote_copy "${groups_path}" "${remote_groups_path}"
remote_copy "${users_path}" "${remote_users_path}"

remote_exec "set -euo pipefail
docker cp $(shell_quote "${remote_accounts_path}") sub2api-standby-postgres:/tmp/openai-oauth-accounts-scheduling.tsv
docker cp $(shell_quote "${remote_group_defs_path}") sub2api-standby-postgres:/tmp/openai-oauth-groups.tsv
docker cp $(shell_quote "${remote_groups_path}") sub2api-standby-postgres:/tmp/openai-oauth-account-groups.tsv
docker cp $(shell_quote "${remote_users_path}") sub2api-standby-postgres:/tmp/api-key-users.tsv
docker exec -i sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE local_openai_oauth_accounts_scheduling (
  id bigint PRIMARY KEY,
  status text,
  schedulable boolean,
  error_message text,
  deleted_at timestamptz,
  rate_limited_at timestamptz,
  rate_limit_reset_at timestamptz,
  overload_until timestamptz,
  session_window_status text,
  temp_unschedulable_until timestamptz,
  temp_unschedulable_reason text,
  rate_multiplier double precision,
  proxy_id bigint,
  updated_at timestamptz
);

CREATE TEMP TABLE local_openai_oauth_group_defs (
  name text PRIMARY KEY,
  description text,
  rate_multiplier numeric,
  is_exclusive boolean,
  status text,
  platform text,
  subscription_type text,
  daily_limit_usd numeric,
  weekly_limit_usd numeric,
  monthly_limit_usd numeric,
  default_validity_days integer,
  image_price_1k numeric,
  image_price_2k numeric,
  image_price_4k numeric,
  claude_code_only boolean,
  model_routing jsonb,
  model_routing_enabled boolean,
  mcp_xml_inject boolean,
  supported_model_scopes jsonb,
  sort_order integer,
  allow_messages_dispatch boolean,
  default_mapped_model text,
  require_oauth_only boolean,
  require_privacy_set boolean,
  messages_dispatch_model_config jsonb,
  hybrid_dispatch jsonb,
  hybrid_dispatch_enabled boolean,
  rpm_limit integer,
  allow_image_generation boolean,
  image_rate_independent boolean,
  image_rate_multiplier numeric,
  models_list_config jsonb,
  updated_at timestamptz
);

CREATE TEMP TABLE local_openai_oauth_account_groups (
  account_id bigint NOT NULL,
  group_name text NOT NULL,
  priority bigint,
  created_at timestamptz,
  PRIMARY KEY (account_id, group_name)
);

CREATE TEMP TABLE local_api_key_users (
  id bigint PRIMARY KEY,
  role text,
  balance numeric,
  concurrency integer,
  status text,
  rpm_limit integer,
  updated_at timestamptz
);

\copy local_openai_oauth_accounts_scheduling FROM '/tmp/openai-oauth-accounts-scheduling.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_openai_oauth_group_defs FROM '/tmp/openai-oauth-groups.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_openai_oauth_account_groups FROM '/tmp/openai-oauth-account-groups.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_api_key_users FROM '/tmp/api-key-users.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH updated AS (
  UPDATE public.users u
  SET role = l.role,
      balance = l.balance,
      concurrency = l.concurrency,
      status = l.status,
      rpm_limit = l.rpm_limit,
      updated_at = GREATEST(u.updated_at, l.updated_at)
  FROM local_api_key_users l
  WHERE u.id = l.id
    AND u.deleted_at IS NULL
  RETURNING u.id
)
SELECT 'synced_api_key_users=' || count(*) FROM updated;

WITH updated_groups AS (
  UPDATE public.groups g
  SET description = l.description,
      rate_multiplier = l.rate_multiplier,
      is_exclusive = l.is_exclusive,
      status = l.status,
      platform = l.platform,
      subscription_type = l.subscription_type,
      daily_limit_usd = l.daily_limit_usd,
      weekly_limit_usd = l.weekly_limit_usd,
      monthly_limit_usd = l.monthly_limit_usd,
      default_validity_days = l.default_validity_days,
      image_price_1k = l.image_price_1k,
      image_price_2k = l.image_price_2k,
      image_price_4k = l.image_price_4k,
      claude_code_only = l.claude_code_only,
      fallback_group_id = NULL,
      model_routing = l.model_routing,
      model_routing_enabled = l.model_routing_enabled,
      fallback_group_id_on_invalid_request = NULL,
      mcp_xml_inject = l.mcp_xml_inject,
      supported_model_scopes = l.supported_model_scopes,
      sort_order = l.sort_order,
      allow_messages_dispatch = l.allow_messages_dispatch,
      default_mapped_model = l.default_mapped_model,
      require_oauth_only = l.require_oauth_only,
      require_privacy_set = l.require_privacy_set,
      messages_dispatch_model_config = l.messages_dispatch_model_config,
      hybrid_dispatch = l.hybrid_dispatch,
      hybrid_dispatch_enabled = l.hybrid_dispatch_enabled,
      gpt_target_group_id = NULL,
      claude_target_group_id = NULL,
      claude_opus_target_group_id = NULL,
      rpm_limit = l.rpm_limit,
      allow_image_generation = l.allow_image_generation,
      image_rate_independent = l.image_rate_independent,
      image_rate_multiplier = l.image_rate_multiplier,
      models_list_config = l.models_list_config,
      updated_at = GREATEST(g.updated_at, l.updated_at)
  FROM local_openai_oauth_group_defs l
  WHERE g.name = l.name
    AND g.deleted_at IS NULL
  RETURNING g.id
),
inserted_groups AS (
  INSERT INTO public.groups (
    name, description, rate_multiplier, is_exclusive, status, platform,
    subscription_type, daily_limit_usd, weekly_limit_usd, monthly_limit_usd,
    default_validity_days, image_price_1k, image_price_2k, image_price_4k,
    claude_code_only, fallback_group_id, model_routing, model_routing_enabled,
    fallback_group_id_on_invalid_request, mcp_xml_inject, supported_model_scopes,
    sort_order, allow_messages_dispatch, default_mapped_model, require_oauth_only,
    require_privacy_set, messages_dispatch_model_config, hybrid_dispatch,
    hybrid_dispatch_enabled, gpt_target_group_id, claude_target_group_id,
    claude_opus_target_group_id, rpm_limit, allow_image_generation,
    image_rate_independent, image_rate_multiplier, models_list_config,
    created_at, updated_at
  )
  SELECT
    l.name, l.description, l.rate_multiplier, l.is_exclusive, l.status, l.platform,
    l.subscription_type, l.daily_limit_usd, l.weekly_limit_usd, l.monthly_limit_usd,
    l.default_validity_days, l.image_price_1k, l.image_price_2k, l.image_price_4k,
    l.claude_code_only, NULL, l.model_routing, l.model_routing_enabled,
    NULL, l.mcp_xml_inject, l.supported_model_scopes,
    l.sort_order, l.allow_messages_dispatch, l.default_mapped_model, l.require_oauth_only,
    l.require_privacy_set, l.messages_dispatch_model_config, l.hybrid_dispatch,
    l.hybrid_dispatch_enabled, NULL, NULL,
    NULL, l.rpm_limit, l.allow_image_generation,
    l.image_rate_independent, l.image_rate_multiplier, l.models_list_config,
    now(), l.updated_at
  FROM local_openai_oauth_group_defs l
  WHERE NOT EXISTS (
    SELECT 1
    FROM public.groups g
    WHERE g.name = l.name
      AND g.deleted_at IS NULL
  )
  RETURNING id
)
SELECT 'synced_openai_oauth_groups updated=' || (SELECT count(*) FROM updated_groups)
  || ' inserted=' || (SELECT count(*) FROM inserted_groups);

WITH updated AS (
  UPDATE public.accounts a
      SET status = CASE
        WHEN '${STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS}' = 'true' THEN l.status
        ELSE a.status
      END,
      schedulable = CASE
        WHEN '${STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS}' = 'true' THEN l.schedulable
        WHEN COALESCE(l.status, 'active') = 'active' THEN l.schedulable
        ELSE a.schedulable
      END,
      error_message = CASE
        WHEN '${STANDBY_SYNC_OPENAI_OAUTH_PERMANENT_ERRORS}' = 'true' THEN l.error_message
        ELSE a.error_message
      END,
      deleted_at = l.deleted_at,
      rate_limited_at = CASE
        WHEN l.updated_at > a.updated_at THEN l.rate_limited_at
        WHEN a.rate_limited_at IS NULL THEN l.rate_limited_at
        WHEN l.rate_limited_at IS NULL THEN a.rate_limited_at
        ELSE GREATEST(a.rate_limited_at, l.rate_limited_at)
      END,
      rate_limit_reset_at = CASE
        WHEN l.updated_at > a.updated_at THEN l.rate_limit_reset_at
        WHEN a.rate_limit_reset_at IS NULL THEN l.rate_limit_reset_at
        WHEN l.rate_limit_reset_at IS NULL THEN a.rate_limit_reset_at
        ELSE GREATEST(a.rate_limit_reset_at, l.rate_limit_reset_at)
      END,
      overload_until = CASE
        WHEN l.updated_at > a.updated_at THEN l.overload_until
        WHEN a.overload_until IS NULL THEN l.overload_until
        WHEN l.overload_until IS NULL THEN a.overload_until
        ELSE GREATEST(a.overload_until, l.overload_until)
      END,
      session_window_status = l.session_window_status,
      temp_unschedulable_until = CASE
        WHEN l.updated_at > a.updated_at THEN l.temp_unschedulable_until
        WHEN a.temp_unschedulable_until IS NULL THEN l.temp_unschedulable_until
        WHEN l.temp_unschedulable_until IS NULL THEN a.temp_unschedulable_until
        ELSE GREATEST(a.temp_unschedulable_until, l.temp_unschedulable_until)
      END,
      temp_unschedulable_reason = CASE
        WHEN l.updated_at > a.updated_at THEN l.temp_unschedulable_reason
        WHEN l.temp_unschedulable_until IS NULL THEN a.temp_unschedulable_reason
        WHEN a.temp_unschedulable_until IS NULL THEN l.temp_unschedulable_reason
        WHEN l.temp_unschedulable_until >= a.temp_unschedulable_until THEN l.temp_unschedulable_reason
        ELSE a.temp_unschedulable_reason
      END,
      rate_multiplier = l.rate_multiplier,
      proxy_id = CASE
        WHEN '${STANDBY_PROXY_POLICY}' = 'clear_all' OR '${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS}' = 'true' THEN NULL
        WHEN l.proxy_id IS NULL THEN NULL
        ELSE COALESCE(NULLIF('${STANDBY_LOCAL_PROXY_ID}','')::bigint, 6)
      END,
      updated_at = GREATEST(a.updated_at, l.updated_at)
  FROM local_openai_oauth_accounts_scheduling l
  WHERE a.id = l.id
    AND a.platform = 'openai'
    AND a.type = 'oauth'
  RETURNING a.id
)
SELECT 'synced_openai_oauth_scheduling_accounts=' || count(*) FROM updated;

DELETE FROM public.account_groups ag
USING public.accounts a
WHERE ag.account_id = a.id
  AND a.platform = 'openai'
  AND a.type = 'oauth';

INSERT INTO public.account_groups (account_id, group_id, priority, created_at)
SELECT lag.account_id, rg.id, lag.priority, lag.created_at
FROM local_openai_oauth_account_groups lag
JOIN public.accounts a
  ON a.id = lag.account_id
 AND a.platform = 'openai'
 AND a.type = 'oauth'
JOIN (
  SELECT DISTINCT ON (name) id, name
  FROM public.groups
  WHERE deleted_at IS NULL
  ORDER BY name, id
) rg ON rg.name = lag.group_name
ON CONFLICT (account_id, group_id) DO UPDATE
SET priority = EXCLUDED.priority;

SELECT 'synced_openai_oauth_account_groups=' || count(*)
FROM public.account_groups ag
JOIN public.accounts a ON a.id = ag.account_id
WHERE a.platform = 'openai'
  AND a.type = 'oauth';

WITH from_group AS (
  SELECT id
  FROM public.groups
  WHERE name = '${STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME_SQL}'
    AND deleted_at IS NULL
  ORDER BY id
  LIMIT 1
),
to_group AS (
  SELECT id
  FROM public.groups
  WHERE name = '${STANDBY_REMAP_API_KEYS_TO_GROUP_NAME_SQL}'
    AND deleted_at IS NULL
  ORDER BY id
  LIMIT 1
),
updated_api_keys AS (
  UPDATE public.api_keys k
  SET group_id = tg.id,
      updated_at = now()
  FROM from_group fg, to_group tg
  WHERE k.deleted_at IS NULL
    AND k.group_id = fg.id
  RETURNING k.id
)
SELECT 'remapped_api_keys_to_standby_group=' || count(*) FROM updated_api_keys;
SQL
docker exec sub2api-standby-postgres rm -f /tmp/openai-oauth-accounts-scheduling.tsv /tmp/openai-oauth-groups.tsv /tmp/openai-oauth-account-groups.tsv /tmp/api-key-users.tsv
rm -f $(shell_quote "${remote_accounts_path}") $(shell_quote "${remote_group_defs_path}") $(shell_quote "${remote_groups_path}") $(shell_quote "${remote_users_path}")
if [[ $(shell_quote "${STANDBY_RESTART_AFTER_SCHEDULING_SYNC}") == true ]]; then
  systemctl restart $(shell_quote "${SERVICE_NAME}")
fi"

echo "OpenAI OAuth scheduling sync completed."
