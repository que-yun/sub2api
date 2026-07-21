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
HOT_GROUP_NAME="${HOT_GROUP_NAME:-codex-warp-verified}"
STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME="${STANDBY_REMAP_API_KEYS_FROM_GROUP_NAME:-${HOT_GROUP_NAME}}"
STANDBY_REMAP_API_KEYS_TO_GROUP_NAME="${STANDBY_REMAP_API_KEYS_TO_GROUP_NAME:-}"
STANDBY_REMAP_API_KEY_GROUP_PAIRS="${STANDBY_REMAP_API_KEY_GROUP_PAIRS:-}"
STANDBY_CLEAR_ROUTE_ACCOUNT_PROXY_IDS="${STANDBY_CLEAR_ROUTE_ACCOUNT_PROXY_IDS:-true}"
STANDBY_RESTART_AFTER_ROUTE_SYNC="${STANDBY_RESTART_AFTER_ROUTE_SYNC:-false}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"

tmp_dir="$(mktemp -d)"
groups_path="${tmp_dir}/route-groups.tsv"
api_keys_path="${tmp_dir}/route-api-keys.tsv"
route_accounts_path="${tmp_dir}/route-accounts.tsv"
account_ids_path="${tmp_dir}/route-account-ids.tsv"
account_groups_path="${tmp_dir}/route-account-groups.tsv"
user_allowed_groups_path="${tmp_dir}/route-user-allowed-groups.tsv"
remote_groups_path="/tmp/sub2api-route-groups.tsv"
remote_api_keys_path="/tmp/sub2api-route-api-keys.tsv"
remote_route_accounts_path="/tmp/sub2api-route-accounts.tsv"
remote_account_ids_path="/tmp/sub2api-route-account-ids.tsv"
remote_account_groups_path="/tmp/sub2api-route-account-groups.tsv"
remote_user_allowed_groups_path="/tmp/sub2api-route-user-allowed-groups.tsv"

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

build_group_remap_values_sql() {
  local pairs="$1"
  local pair from_group to_group
  local values=()

  while IFS= read -r pair; do
    [[ -z "${pair}" ]] && continue
    if [[ "${pair}" != *:* ]]; then
      echo "Invalid STANDBY_REMAP_API_KEY_GROUP_PAIRS item: ${pair}" >&2
      exit 1
    fi
    from_group="${pair%%:*}"
    to_group="${pair#*:}"
    if [[ -z "${from_group}" || -z "${to_group}" ]]; then
      echo "Invalid STANDBY_REMAP_API_KEY_GROUP_PAIRS item: ${pair}" >&2
      exit 1
    fi
    values+=("('$(sql_escape "${from_group}")', '$(sql_escape "${to_group}")')")
  done < <(printf "%s" "${pairs}" | tr ',' '\n')

  if (( ${#values[@]} == 0 )); then
    printf "SELECT NULL::text AS from_group_name, NULL::text AS to_group_name WHERE false"
    return
  fi

  local IFS=","
  printf "VALUES %s" "${values[*]}"
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

scp_args=(
  -o BatchMode=yes
  -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}"
  -o ServerAliveInterval=15
  -o ServerAliveCountMax=4
  -o StrictHostKeyChecking=accept-new
)
if [[ -n "${SSH_PORT}" ]]; then
  scp_args+=(-P "${SSH_PORT}")
fi
if [[ -n "${SSH_IDENTITY_FILE}" ]]; then
  scp_args+=(-i "${SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes)
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
  run_with_timeout "${REMOTE_COMMAND_TIMEOUT}" scp "${scp_args[@]}" "$1" "${REMOTE_COPY_HOST}:$2"
}

remote_copy() {
  retry_remote "remote_copy $1 -> $2" remote_copy_once "$1" "$2"
}

remap_values_sql="$(build_group_remap_values_sql "${STANDBY_REMAP_API_KEY_GROUP_PAIRS}")"

echo "Exporting local route configuration ..."
echo "Route API key proxy clearing: ${STANDBY_CLEAR_ROUTE_ACCOUNT_PROXY_IDS}"
local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    g.name,
    g.description,
    g.rate_multiplier,
    g.is_exclusive,
    g.status,
    g.created_at,
    g.updated_at,
    g.deleted_at,
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
    fg.name AS fallback_group_name,
    g.model_routing,
    g.model_routing_enabled,
    fig.name AS fallback_group_on_invalid_request_name,
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
    gtg.name AS gpt_target_group_name,
    ctg.name AS claude_target_group_name,
    cotg.name AS claude_opus_target_group_name,
    g.rpm_limit,
    g.allow_image_generation,
    g.image_rate_independent,
    g.image_rate_multiplier,
    g.models_list_config
  FROM public.groups g
  LEFT JOIN public.groups fg ON fg.id = g.fallback_group_id AND fg.deleted_at IS NULL
  LEFT JOIN public.groups fig ON fig.id = g.fallback_group_id_on_invalid_request AND fig.deleted_at IS NULL
  LEFT JOIN public.groups gtg ON gtg.id = g.gpt_target_group_id AND gtg.deleted_at IS NULL
  LEFT JOIN public.groups ctg ON ctg.id = g.claude_target_group_id AND ctg.deleted_at IS NULL
  LEFT JOIN public.groups cotg ON cotg.id = g.claude_opus_target_group_id AND cotg.deleted_at IS NULL
  WHERE g.deleted_at IS NULL
    AND g.id IN (
      -- name is not unique once soft-deleted rows remain; temp import table PKs on name.
      SELECT DISTINCT ON (name) id
      FROM public.groups
      WHERE deleted_at IS NULL
      ORDER BY name, updated_at DESC NULLS LAST, id DESC
    )
  ORDER BY g.id
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${groups_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    k.id,
    k.user_id,
    k.key,
    k.name,
    g.name AS group_name,
    k.status,
    k.created_at,
    k.updated_at,
    k.deleted_at,
    k.ip_whitelist,
    k.ip_blacklist,
    k.quota,
    k.quota_used,
    k.expires_at,
    k.last_used_at,
    k.rate_limit_5h,
    k.rate_limit_1d,
    k.rate_limit_7d,
    k.usage_5h,
    k.usage_1d,
    k.usage_7d,
    k.window_5h_start,
    k.window_1d_start,
    k.window_7d_start,
    COALESCE(
      (
        SELECT jsonb_agg(fg.name ORDER BY ord)
        FROM jsonb_array_elements_text(k.fallback_group_ids) WITH ORDINALITY AS f(group_id_text, ord)
        JOIN public.groups fg ON fg.id = f.group_id_text::bigint
      ),
      '[]'::jsonb
    ) AS fallback_group_names
  FROM public.api_keys k
  LEFT JOIN public.groups g ON g.id = k.group_id
  ORDER BY k.id
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${api_keys_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT DISTINCT
    a.id,
    a.name,
    a.platform,
    a.type,
    a.credentials,
    a.extra,
    CASE
      WHEN '${STANDBY_CLEAR_ROUTE_ACCOUNT_PROXY_IDS}' = 'true' THEN NULL
      ELSE a.proxy_id
    END AS proxy_id,
    a.concurrency,
    a.priority,
    a.status,
    a.created_at,
    a.updated_at,
    a.deleted_at,
    a.schedulable,
    a.notes,
    a.expires_at,
    a.auto_pause_on_expired,
    a.rate_multiplier,
    a.load_factor
  FROM public.accounts a
  JOIN public.account_groups ag ON ag.account_id = a.id
  JOIN public.groups g ON g.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND g.deleted_at IS NULL
    AND a.type = 'apikey'
  ORDER BY a.id
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${route_accounts_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT id
  FROM public.accounts
  ORDER BY id
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${account_ids_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    ag.account_id,
    g.name AS group_name,
    ag.priority,
    ag.created_at
  FROM public.account_groups ag
  JOIN public.groups g ON g.id = ag.group_id
  JOIN public.accounts a ON a.id = ag.account_id
  WHERE a.deleted_at IS NULL
    AND g.deleted_at IS NULL
  ORDER BY ag.account_id, g.name
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${account_groups_path}"

local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    uag.user_id,
    g.name AS group_name,
    uag.created_at
  FROM public.user_allowed_groups uag
  JOIN public.groups g ON g.id = uag.group_id
  WHERE g.deleted_at IS NULL
  ORDER BY uag.user_id, g.name
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${user_allowed_groups_path}"

echo "Exported groups=$(wc -l < "${groups_path}" | tr -d ' ') api_keys=$(wc -l < "${api_keys_path}" | tr -d ' ') route_accounts=$(wc -l < "${route_accounts_path}" | tr -d ' ') account_groups=$(wc -l < "${account_groups_path}" | tr -d ' ') user_allowed_groups=$(wc -l < "${user_allowed_groups_path}" | tr -d ' ')"

echo "Uploading route configuration to VPS ..."
remote_copy "${groups_path}" "${remote_groups_path}"
remote_copy "${api_keys_path}" "${remote_api_keys_path}"
remote_copy "${route_accounts_path}" "${remote_route_accounts_path}"
remote_copy "${account_ids_path}" "${remote_account_ids_path}"
remote_copy "${account_groups_path}" "${remote_account_groups_path}"
remote_copy "${user_allowed_groups_path}" "${remote_user_allowed_groups_path}"

remote_exec "set -euo pipefail
docker cp $(shell_quote "${remote_groups_path}") sub2api-standby-postgres:/tmp/route-groups.tsv
docker cp $(shell_quote "${remote_api_keys_path}") sub2api-standby-postgres:/tmp/route-api-keys.tsv
docker cp $(shell_quote "${remote_route_accounts_path}") sub2api-standby-postgres:/tmp/route-accounts.tsv
docker cp $(shell_quote "${remote_account_ids_path}") sub2api-standby-postgres:/tmp/route-account-ids.tsv
docker cp $(shell_quote "${remote_account_groups_path}") sub2api-standby-postgres:/tmp/route-account-groups.tsv
docker cp $(shell_quote "${remote_user_allowed_groups_path}") sub2api-standby-postgres:/tmp/route-user-allowed-groups.tsv
docker exec -i sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE local_route_groups (
  name text PRIMARY KEY,
  description text,
  rate_multiplier numeric,
  is_exclusive boolean,
  status text,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
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
  fallback_group_name text,
  model_routing jsonb,
  model_routing_enabled boolean,
  fallback_group_on_invalid_request_name text,
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
  gpt_target_group_name text,
  claude_target_group_name text,
  claude_opus_target_group_name text,
  rpm_limit integer,
  allow_image_generation boolean,
  image_rate_independent boolean,
  image_rate_multiplier numeric,
  models_list_config jsonb
);

CREATE TEMP TABLE local_route_api_keys (
  id bigint PRIMARY KEY,
  user_id bigint NOT NULL,
  key text NOT NULL,
  name text NOT NULL,
  group_name text,
  status text,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  ip_whitelist jsonb,
  ip_blacklist jsonb,
  quota numeric,
  quota_used numeric,
  expires_at timestamptz,
  last_used_at timestamptz,
  rate_limit_5h numeric,
  rate_limit_1d numeric,
  rate_limit_7d numeric,
  usage_5h numeric,
  usage_1d numeric,
  usage_7d numeric,
  window_5h_start timestamptz,
  window_1d_start timestamptz,
  window_7d_start timestamptz,
  fallback_group_names jsonb
);

CREATE TEMP TABLE local_route_accounts (
  id bigint PRIMARY KEY,
  name text NOT NULL,
  platform text NOT NULL,
  type text NOT NULL,
  credentials jsonb,
  extra jsonb,
  proxy_id bigint,
  concurrency integer,
  priority integer,
  status text,
  created_at timestamptz,
  updated_at timestamptz,
  deleted_at timestamptz,
  schedulable boolean,
  notes text,
  expires_at timestamptz,
  auto_pause_on_expired boolean,
  rate_multiplier numeric,
  load_factor integer
);

CREATE TEMP TABLE local_route_account_ids (
  id bigint PRIMARY KEY
);

CREATE TEMP TABLE local_route_account_groups (
  account_id bigint NOT NULL,
  group_name text NOT NULL,
  priority bigint,
  created_at timestamptz,
  PRIMARY KEY (account_id, group_name)
);

CREATE TEMP TABLE local_route_user_allowed_groups (
  user_id bigint NOT NULL,
  group_name text NOT NULL,
  created_at timestamptz,
  PRIMARY KEY (user_id, group_name)
);

\copy local_route_groups FROM '/tmp/route-groups.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_route_api_keys FROM '/tmp/route-api-keys.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_route_accounts FROM '/tmp/route-accounts.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_route_account_ids FROM '/tmp/route-account-ids.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_route_account_groups FROM '/tmp/route-account-groups.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
\copy local_route_user_allowed_groups FROM '/tmp/route-user-allowed-groups.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH updated AS (
  UPDATE public.groups g
  SET description = l.description,
      rate_multiplier = l.rate_multiplier,
      is_exclusive = l.is_exclusive,
      status = l.status,
      created_at = LEAST(g.created_at, l.created_at),
      updated_at = GREATEST(g.updated_at, l.updated_at),
      deleted_at = l.deleted_at,
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
      model_routing = l.model_routing,
      model_routing_enabled = l.model_routing_enabled,
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
      rpm_limit = l.rpm_limit,
      allow_image_generation = l.allow_image_generation,
      image_rate_independent = l.image_rate_independent,
      image_rate_multiplier = l.image_rate_multiplier,
      models_list_config = l.models_list_config
  FROM local_route_groups l
  WHERE g.name = l.name
    AND g.deleted_at IS NULL
  RETURNING g.id
),
inserted AS (
  INSERT INTO public.groups (
    name, description, rate_multiplier, is_exclusive, status, created_at,
    updated_at, deleted_at, platform, subscription_type, daily_limit_usd,
    weekly_limit_usd, monthly_limit_usd, default_validity_days, image_price_1k,
    image_price_2k, image_price_4k, claude_code_only, model_routing,
    model_routing_enabled, mcp_xml_inject, supported_model_scopes, sort_order,
    allow_messages_dispatch, default_mapped_model, require_oauth_only,
    require_privacy_set, messages_dispatch_model_config, hybrid_dispatch,
    hybrid_dispatch_enabled, rpm_limit, allow_image_generation,
    image_rate_independent, image_rate_multiplier, models_list_config
  )
  SELECT
    l.name, l.description, l.rate_multiplier, l.is_exclusive, l.status,
    COALESCE(l.created_at, now()), COALESCE(l.updated_at, now()), l.deleted_at,
    l.platform, l.subscription_type, l.daily_limit_usd, l.weekly_limit_usd,
    l.monthly_limit_usd, l.default_validity_days, l.image_price_1k,
    l.image_price_2k, l.image_price_4k, l.claude_code_only, l.model_routing,
    l.model_routing_enabled, l.mcp_xml_inject, l.supported_model_scopes,
    l.sort_order, l.allow_messages_dispatch, l.default_mapped_model,
    l.require_oauth_only, l.require_privacy_set, l.messages_dispatch_model_config,
    l.hybrid_dispatch, l.hybrid_dispatch_enabled, l.rpm_limit,
    l.allow_image_generation, l.image_rate_independent, l.image_rate_multiplier,
    l.models_list_config
  FROM local_route_groups l
  WHERE NOT EXISTS (
    SELECT 1 FROM public.groups g
    WHERE g.name = l.name
      AND g.deleted_at IS NULL
  )
  RETURNING id
)
SELECT 'synced_route_groups updated=' || (SELECT count(*) FROM updated)
  || ' inserted=' || (SELECT count(*) FROM inserted);

UPDATE public.groups g
SET fallback_group_id = fg.id,
    fallback_group_id_on_invalid_request = fig.id,
    gpt_target_group_id = gtg.id,
    claude_target_group_id = ctg.id,
    claude_opus_target_group_id = cotg.id
FROM local_route_groups l
LEFT JOIN public.groups fg ON fg.name = l.fallback_group_name AND fg.deleted_at IS NULL
LEFT JOIN public.groups fig ON fig.name = l.fallback_group_on_invalid_request_name AND fig.deleted_at IS NULL
LEFT JOIN public.groups gtg ON gtg.name = l.gpt_target_group_name AND gtg.deleted_at IS NULL
LEFT JOIN public.groups ctg ON ctg.name = l.claude_target_group_name AND ctg.deleted_at IS NULL
LEFT JOIN public.groups cotg ON cotg.name = l.claude_opus_target_group_name AND cotg.deleted_at IS NULL
WHERE g.name = l.name
  AND g.deleted_at IS NULL;

WITH stale AS (
  UPDATE public.groups g
  SET deleted_at = now(),
      status = CASE WHEN g.status = 'active' THEN 'disabled' ELSE g.status END,
      updated_at = now()
  WHERE g.deleted_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM local_route_groups l WHERE l.name = g.name)
  RETURNING g.id
)
SELECT 'soft_deleted_stale_groups=' || count(*) FROM stale;

WITH prepared_route_accounts AS (
  SELECT l.*
  FROM local_route_accounts l
  WHERE l.type = 'apikey'
),
updated AS (
  UPDATE public.accounts a
  SET name = p.name,
      platform = p.platform,
      type = p.type,
      credentials = p.credentials,
      extra = p.extra,
      proxy_id = p.proxy_id,
      concurrency = p.concurrency,
      priority = p.priority,
      status = p.status,
      created_at = LEAST(a.created_at, p.created_at),
      updated_at = GREATEST(a.updated_at, p.updated_at),
      deleted_at = p.deleted_at,
      schedulable = COALESCE(p.schedulable, true),
      notes = p.notes,
      expires_at = p.expires_at,
      auto_pause_on_expired = p.auto_pause_on_expired,
      rate_multiplier = p.rate_multiplier,
      load_factor = p.load_factor
  FROM prepared_route_accounts p
  WHERE a.id = p.id
    AND a.type = 'apikey'
  RETURNING a.id
),
inserted AS (
  INSERT INTO public.accounts (
    id, name, platform, type, credentials, extra, proxy_id, concurrency,
    priority, status, error_message, last_used_at, created_at, updated_at,
    deleted_at, schedulable, rate_limited_at, rate_limit_reset_at,
    overload_until, session_window_start, session_window_end,
    session_window_status, temp_unschedulable_until,
    temp_unschedulable_reason, notes, expires_at, auto_pause_on_expired,
    rate_multiplier, load_factor
  )
  SELECT
    p.id, p.name, p.platform, p.type, COALESCE(p.credentials, '{}'::jsonb),
    COALESCE(p.extra, '{}'::jsonb), p.proxy_id, COALESCE(p.concurrency, 1),
    COALESCE(p.priority, 0), COALESCE(p.status, 'active'), NULL, NULL,
    COALESCE(p.created_at, now()), COALESCE(p.updated_at, now()), p.deleted_at,
    COALESCE(p.schedulable, true), NULL, NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, p.notes, p.expires_at, COALESCE(p.auto_pause_on_expired, false),
    COALESCE(p.rate_multiplier, 1), p.load_factor
  FROM prepared_route_accounts p
  WHERE NOT EXISTS (SELECT 1 FROM public.accounts a WHERE a.id = p.id)
  RETURNING id
)
SELECT 'synced_route_accounts updated=' || (SELECT count(*) FROM updated)
  || ' inserted=' || (SELECT count(*) FROM inserted);

WITH cleared AS (
  UPDATE public.accounts a
  SET proxy_id = NULL,
      updated_at = now()
  WHERE a.platform = 'openai'
    AND a.type = 'apikey'
    AND a.proxy_id IS NOT NULL
  RETURNING a.id
)
SELECT 'cleared_route_apikey_proxy_ids=' || count(*) FROM cleared;

DO \$\$
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
      'route sync invariant failed: standby OpenAI API key accounts must not keep proxy_id; remaining=%, sample=%',
      remaining_count, sample;
  END IF;
END \$\$;

SELECT 'verified_route_apikey_proxy_ids_cleared=0';

SELECT setval(pg_get_serial_sequence('public.accounts', 'id'), GREATEST((SELECT COALESCE(MAX(id), 1) FROM public.accounts), 1), true);

DELETE FROM public.account_groups ag
USING local_route_account_ids lai
WHERE ag.account_id = lai.id;

INSERT INTO public.account_groups (account_id, group_id, priority, created_at)
SELECT lag.account_id, rg.id, lag.priority, COALESCE(lag.created_at, now())
FROM local_route_account_groups lag
JOIN public.accounts a ON a.id = lag.account_id
JOIN public.groups rg ON rg.name = lag.group_name AND rg.deleted_at IS NULL
ON CONFLICT (account_id, group_id) DO UPDATE
SET priority = EXCLUDED.priority;

SELECT 'synced_account_groups=' || count(*) FROM public.account_groups;

WITH api_key_group_remaps(from_group_name, to_group_name) AS (
  ${remap_values_sql}
),
mapped_api_keys AS (
  SELECT
    l.*,
    COALESCE(r.to_group_name, l.group_name) AS mapped_group_name
  FROM local_route_api_keys l
  LEFT JOIN api_key_group_remaps r ON r.from_group_name = l.group_name
),
prepared_api_keys AS (
  SELECT
    l.*,
    rg.id AS mapped_group_id,
    COALESCE(
      (
        SELECT jsonb_agg(frg.id ORDER BY f.ord)
        FROM jsonb_array_elements_text(COALESCE(l.fallback_group_names, '[]'::jsonb)) WITH ORDINALITY AS f(group_name, ord)
        LEFT JOIN api_key_group_remaps fr
          ON fr.from_group_name = f.group_name
        JOIN public.groups frg
          ON frg.name = COALESCE(fr.to_group_name, f.group_name)
         AND frg.deleted_at IS NULL
      ),
      '[]'::jsonb
    ) AS mapped_fallback_group_ids
  FROM mapped_api_keys l
  LEFT JOIN public.groups rg ON rg.name = l.mapped_group_name AND rg.deleted_at IS NULL
),
updated AS (
  UPDATE public.api_keys k
  SET user_id = p.user_id,
      key = p.key,
      name = p.name,
      group_id = p.mapped_group_id,
      status = p.status,
      created_at = LEAST(k.created_at, p.created_at),
      updated_at = GREATEST(k.updated_at, p.updated_at),
      deleted_at = p.deleted_at,
      ip_whitelist = p.ip_whitelist,
      ip_blacklist = p.ip_blacklist,
      quota = p.quota,
      expires_at = p.expires_at,
      rate_limit_5h = p.rate_limit_5h,
      rate_limit_1d = p.rate_limit_1d,
      rate_limit_7d = p.rate_limit_7d,
      fallback_group_ids = p.mapped_fallback_group_ids
  FROM prepared_api_keys p
  WHERE k.id = p.id
  RETURNING k.id
),
inserted AS (
  INSERT INTO public.api_keys (
    id, user_id, key, name, group_id, status, created_at, updated_at, deleted_at,
    ip_whitelist, ip_blacklist, quota, quota_used, expires_at, last_used_at,
    rate_limit_5h, rate_limit_1d, rate_limit_7d, usage_5h, usage_1d, usage_7d,
    window_5h_start, window_1d_start, window_7d_start, fallback_group_ids
  )
  SELECT
    p.id, p.user_id, p.key, p.name, p.mapped_group_id, p.status,
    COALESCE(p.created_at, now()), COALESCE(p.updated_at, now()), p.deleted_at,
    COALESCE(p.ip_whitelist, '[]'::jsonb), COALESCE(p.ip_blacklist, '[]'::jsonb),
    COALESCE(p.quota, 0), COALESCE(p.quota_used, 0), p.expires_at, p.last_used_at,
    COALESCE(p.rate_limit_5h, 0), COALESCE(p.rate_limit_1d, 0), COALESCE(p.rate_limit_7d, 0),
    COALESCE(p.usage_5h, 0), COALESCE(p.usage_1d, 0), COALESCE(p.usage_7d, 0),
    p.window_5h_start, p.window_1d_start, p.window_7d_start, p.mapped_fallback_group_ids
  FROM prepared_api_keys p
  WHERE NOT EXISTS (SELECT 1 FROM public.api_keys k WHERE k.id = p.id)
    AND NOT EXISTS (SELECT 1 FROM public.api_keys k WHERE k.key = p.key)
  RETURNING id
),
stale AS (
  UPDATE public.api_keys k
  SET deleted_at = now(),
      status = CASE WHEN k.status = 'active' THEN 'disabled' ELSE k.status END,
      updated_at = now()
  WHERE k.deleted_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM local_route_api_keys l WHERE l.id = k.id OR l.key = k.key)
  RETURNING k.id
)
SELECT 'synced_api_keys updated=' || (SELECT count(*) FROM updated)
  || ' inserted=' || (SELECT count(*) FROM inserted)
  || ' soft_deleted_stale=' || (SELECT count(*) FROM stale);

WITH api_key_group_remaps(from_group_name, to_group_name) AS (
  ${remap_values_sql}
),
resolved_remaps AS (
  SELECT fg.id AS from_group_id, tg.id AS to_group_id, r.from_group_name, r.to_group_name
  FROM api_key_group_remaps r
  JOIN public.groups fg ON fg.name = r.from_group_name AND fg.deleted_at IS NULL
  JOIN public.groups tg ON tg.name = r.to_group_name AND tg.deleted_at IS NULL
),
remapped_primary AS (
  UPDATE public.api_keys k
  SET group_id = r.to_group_id,
      updated_at = now()
  FROM resolved_remaps r
  WHERE k.deleted_at IS NULL
    AND k.group_id = r.from_group_id
  RETURNING k.id, r.from_group_name, r.to_group_name
),
remapped_fallback AS (
  UPDATE public.api_keys k
  SET fallback_group_ids = COALESCE((
        SELECT jsonb_agg(
          COALESCE(r.to_group_id, elem.group_id_text::bigint)
          ORDER BY elem.ord
        )
        FROM jsonb_array_elements_text(COALESCE(k.fallback_group_ids, '[]'::jsonb)) WITH ORDINALITY AS elem(group_id_text, ord)
        LEFT JOIN resolved_remaps r ON r.from_group_id = elem.group_id_text::bigint
      ), '[]'::jsonb),
      updated_at = now()
  WHERE k.deleted_at IS NULL
    AND EXISTS (
      SELECT 1
      FROM jsonb_array_elements_text(COALESCE(k.fallback_group_ids, '[]'::jsonb)) AS elem(group_id_text)
      JOIN resolved_remaps r ON r.from_group_id = elem.group_id_text::bigint
    )
  RETURNING k.id
)
SELECT 'remapped_api_key_groups primary=' || (SELECT count(*) FROM remapped_primary)
  || ' fallback=' || (SELECT count(*) FROM remapped_fallback)
  || ' pairs=' || COALESCE((SELECT string_agg(from_group_name || '->' || to_group_name, ', ' ORDER BY from_group_name) FROM resolved_remaps), '');

SELECT setval(pg_get_serial_sequence('public.api_keys', 'id'), GREATEST((SELECT COALESCE(MAX(id), 1) FROM public.api_keys), 1), true);

DELETE FROM public.user_allowed_groups uag
WHERE uag.user_id IN (
  SELECT user_id FROM local_route_user_allowed_groups
  UNION
  SELECT user_id FROM local_route_api_keys
);

INSERT INTO public.user_allowed_groups (user_id, group_id, created_at)
SELECT luag.user_id, g.id, COALESCE(luag.created_at, now())
FROM local_route_user_allowed_groups luag
JOIN public.groups g ON g.name = luag.group_name AND g.deleted_at IS NULL
ON CONFLICT (user_id, group_id) DO NOTHING;

SELECT 'synced_user_allowed_groups=' || count(*) FROM public.user_allowed_groups;

SELECT
  'route_summary active_groups=' || (SELECT count(*) FROM public.groups WHERE deleted_at IS NULL)
  || ' active_api_keys=' || (SELECT count(*) FROM public.api_keys WHERE deleted_at IS NULL)
  || ' active_account_groups=' || (SELECT count(*) FROM public.account_groups ag JOIN public.accounts a ON a.id = ag.account_id WHERE a.deleted_at IS NULL);
SQL
docker exec sub2api-standby-postgres rm -f /tmp/route-groups.tsv /tmp/route-api-keys.tsv /tmp/route-accounts.tsv /tmp/route-account-ids.tsv /tmp/route-account-groups.tsv /tmp/route-user-allowed-groups.tsv
rm -f $(shell_quote "${remote_groups_path}") $(shell_quote "${remote_api_keys_path}") $(shell_quote "${remote_route_accounts_path}") $(shell_quote "${remote_account_ids_path}") $(shell_quote "${remote_account_groups_path}") $(shell_quote "${remote_user_allowed_groups_path}")
if [[ $(shell_quote "${STANDBY_RESTART_AFTER_ROUTE_SYNC}") == true ]]; then
  systemctl restart $(shell_quote "${SERVICE_NAME}")
fi"

echo "Route configuration sync completed."
