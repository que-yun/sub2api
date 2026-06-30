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

HOT_GROUP_NAME="${HOT_GROUP_NAME:-codex-warp-verified}"
COOLDOWN_GROUP_NAME="${COOLDOWN_GROUP_NAME:-codex-warp-cooldown}"
DEAD_GROUP_NAME="${DEAD_GROUP_NAME:-codex-warp-dead}"
SOURCE_GROUP_NAME="${SOURCE_GROUP_NAME:-codex-warp-source}"
BUG_POOL_GROUP_NAME="${BUG_POOL_GROUP_NAME:-free-bug-gt100-20260430}"
DEFAULT_GROUP_NAME="${DEFAULT_GROUP_NAME:-openai-default}"
SOURCE_SEED_ACCOUNT_IDS="${SOURCE_SEED_ACCOUNT_IDS:-542,595,590,585,577,572,569,567,563,537,533,531,521,513,507,502,501,495,493,470,466,465,458,456,443,441,432,429,421,419}"
AUTO_ADOPT_LOOKBACK_HOURS="${AUTO_ADOPT_LOOKBACK_HOURS:-12}"
DEFAULT_PROXY_ID="${DEFAULT_PROXY_ID:-${OPENAI_OAUTH_DEFAULT_PROXY_ID:-6}}"
DEFAULT_PROXY_NAME="${DEFAULT_PROXY_NAME:-${OPENAI_OAUTH_DEFAULT_PROXY_NAME:-host-socks5-7891}}"
DEFAULT_PROXY_HOST="${DEFAULT_PROXY_HOST:-${OPENAI_OAUTH_DEFAULT_PROXY_HOST:-host.docker.internal}}"
DEFAULT_PROXY_PORT="${DEFAULT_PROXY_PORT:-${OPENAI_OAUTH_DEFAULT_PROXY_PORT:-7891}}"
DEFAULT_PROXY_PROTOCOL="${DEFAULT_PROXY_PROTOCOL:-${OPENAI_OAUTH_DEFAULT_PROXY_PROTOCOL:-socks5h}}"

# Canonical model_mapping template applied to warp-pool accounts that are missing
# the explicit gpt-5.5 entry. The hard gate in account.go requires this key, and
# blindly inserting only {"gpt-5.5":"gpt-5.5"} onto an empty mapping would lock
# the account to gpt-5.5 only. So we apply the full template when mapping is
# empty, and otherwise just append the gpt-5.5 key — see SQL below.
CANONICAL_MODEL_MAPPING_JSON="${CANONICAL_MODEL_MAPPING_JSON:-{\"o1\":\"o1\",\"o3\":\"o3\",\"gpt-4\":\"gpt-4\",\"gpt-5\":\"gpt-5\",\"gpt-4o\":\"gpt-4o\",\"o1-pro\":\"o1-pro\",\"o3-pro\":\"o3-pro\",\"gpt-4.1\":\"gpt-4.1\",\"gpt-5.1\":\"gpt-5.1\",\"gpt-5.2\":\"gpt-5.2\",\"gpt-5.4\":\"gpt-5.5\",\"gpt-5.5\":\"gpt-5.5\",\"o1-mini\":\"o1-mini\",\"o3-mini\":\"o3-mini\",\"o4-mini\":\"o4-mini\",\"gpt-5-pro\":\"gpt-5-pro\",\"gpt-5-chat\":\"gpt-5-chat\",\"gpt-5-mini\":\"gpt-5-mini\",\"gpt-5-nano\":\"gpt-5-nano\",\"o1-preview\":\"o1-preview\",\"gpt-4-turbo\":\"gpt-4-turbo\",\"gpt-4o-mini\":\"gpt-4o-mini\",\"gpt-5-codex\":\"gpt-5-codex\",\"gpt-5.2-pro\":\"gpt-5.2-pro\",\"gpt-4.1-mini\":\"gpt-4.1-mini\",\"gpt-4.1-nano\":\"gpt-4.1-nano\",\"gpt-5.4-mini\":\"gpt-5.4-mini\",\"gpt-5.4-nano\":\"gpt-5.4-nano\",\"gpt-3.5-turbo\":\"gpt-3.5-turbo\",\"gpt-5.1-codex\":\"gpt-5.1-codex\",\"gpt-5.2-codex\":\"gpt-5.2-codex\",\"gpt-5.3-codex\":\"gpt-5.3-codex\",\"gpt-4.5-preview\":\"gpt-4.5-preview\",\"gpt-5-2025-08-07\":\"gpt-5-2025-08-07\",\"chatgpt-4o-latest\":\"chatgpt-4o-latest\",\"gpt-3.5-turbo-16k\":\"gpt-3.5-turbo-16k\",\"gpt-4o-2024-08-06\":\"gpt-4o-2024-08-06\",\"gpt-4o-2024-11-20\":\"gpt-4o-2024-11-20\",\"gpt-5-chat-latest\":\"gpt-5-chat-latest\",\"gpt-5.1-codex-max\":\"gpt-5.1-codex-max\",\"gpt-3.5-turbo-0125\":\"gpt-3.5-turbo-0125\",\"gpt-3.5-turbo-1106\":\"gpt-3.5-turbo-1106\",\"gpt-5.1-2025-11-13\":\"gpt-5.1-2025-11-13\",\"gpt-5.1-codex-mini\":\"gpt-5.1-codex-mini\",\"gpt-5.2-2025-12-11\":\"gpt-5.2-2025-12-11\",\"gpt-5.4-2026-03-05\":\"gpt-5.4-2026-03-05\",\"gpt-4-turbo-preview\":\"gpt-4-turbo-preview\",\"gpt-5.1-chat-latest\":\"gpt-5.1-chat-latest\",\"gpt-5.2-chat-latest\":\"gpt-5.2-chat-latest\",\"gpt-5.3-codex-spark\":\"gpt-5.3-codex-spark\",\"gpt-4o-audio-preview\":\"gpt-4o-audio-preview\",\"gpt-5-pro-2025-10-06\":\"gpt-5-pro-2025-10-06\",\"gpt-5-mini-2025-08-07\":\"gpt-5-mini-2025-08-07\",\"gpt-5-nano-2025-08-07\":\"gpt-5-nano-2025-08-07\",\"gpt-4o-mini-2024-07-18\":\"gpt-4o-mini-2024-07-18\",\"gpt-5.2-pro-2025-12-11\":\"gpt-5.2-pro-2025-12-11\",\"gpt-4o-realtime-preview\":\"gpt-4o-realtime-preview\"}}"

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

require_non_negative_integer() {
  local name="$1"
  local value="$2"
  if [[ ! "$value" =~ '^[0-9]+$' ]]; then
    echo "${name} must be a non-negative integer, got: ${value}" >&2
    exit 1
  fi
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

HOT_GROUP_NAME_SQL=$(sql_escape "$HOT_GROUP_NAME")
COOLDOWN_GROUP_NAME_SQL=$(sql_escape "$COOLDOWN_GROUP_NAME")
DEAD_GROUP_NAME_SQL=$(sql_escape "$DEAD_GROUP_NAME")
SOURCE_GROUP_NAME_SQL=$(sql_escape "$SOURCE_GROUP_NAME")
BUG_POOL_GROUP_NAME_SQL=$(sql_escape "$BUG_POOL_GROUP_NAME")
DEFAULT_GROUP_NAME_SQL=$(sql_escape "$DEFAULT_GROUP_NAME")
SOURCE_SEED_ACCOUNT_IDS_SQL=$(sql_escape "$SOURCE_SEED_ACCOUNT_IDS")
DEFAULT_PROXY_ID_SQL=$(sql_escape "$DEFAULT_PROXY_ID")
DEFAULT_PROXY_NAME_SQL=$(sql_escape "$DEFAULT_PROXY_NAME")
DEFAULT_PROXY_HOST_SQL=$(sql_escape "$DEFAULT_PROXY_HOST")
DEFAULT_PROXY_PROTOCOL_SQL=$(sql_escape "$DEFAULT_PROXY_PROTOCOL")
CANONICAL_MODEL_MAPPING_JSON_SQL=$(sql_escape "$CANONICAL_MODEL_MAPPING_JSON")

DEFAULT_PROXY_ID_MATCH_SQL="FALSE"
DEFAULT_PROXY_ID_ORDER_SQL=""
if [[ -n "$DEFAULT_PROXY_ID" && "$DEFAULT_PROXY_ID" =~ '^[0-9]+$' ]]; then
  DEFAULT_PROXY_ID_MATCH_SQL="p.id = ${DEFAULT_PROXY_ID}"
  DEFAULT_PROXY_ID_ORDER_SQL=$'      WHEN p.id = '"${DEFAULT_PROXY_ID}"$' THEN 0\n'
fi

if ! has_accounts_runtime_columns; then
  log_info "sync skip: accounts runtime columns missing, waiting for sub2api migrations"
  exit 0
fi

SQL=$(cat <<SQL_EOF
BEGIN;

INSERT INTO groups (
  name,
  description,
  rate_multiplier,
  is_exclusive,
  status,
  platform,
  subscription_type,
  default_validity_days,
  claude_code_only,
  model_routing,
  model_routing_enabled,
  mcp_xml_inject,
  supported_model_scopes,
  sort_order,
  allow_messages_dispatch,
  default_mapped_model,
  require_oauth_only,
  require_privacy_set,
  messages_dispatch_model_config
)
SELECT
  '${SOURCE_GROUP_NAME_SQL}',
  'Static source pool for the existing stable OpenAI OAuth key',
  g.rate_multiplier,
  g.is_exclusive,
  g.status,
  g.platform,
  g.subscription_type,
  g.default_validity_days,
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
  g.messages_dispatch_model_config
FROM groups g
WHERE g.name = '${HOT_GROUP_NAME_SQL}'
  AND g.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM groups WHERE name = '${SOURCE_GROUP_NAME_SQL}' AND deleted_at IS NULL
  );

WITH source_group AS (
  SELECT id
  FROM groups
  WHERE name = '${SOURCE_GROUP_NAME_SQL}'
    AND deleted_at IS NULL
),
auto_adopt_candidates AS (
  SELECT a.id
  FROM accounts a
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND a.status = 'active'
    AND (
      a.created_at >= NOW() - make_interval(hours => ${AUTO_ADOPT_LOOKBACK_HOURS})
      OR EXISTS (
        SELECT 1
        FROM account_groups ag
        JOIN groups g ON g.id = ag.group_id
        WHERE ag.account_id = a.id
          AND g.deleted_at IS NULL
          AND g.name = '${DEFAULT_GROUP_NAME_SQL}'
      )
    )
    AND NOT EXISTS (
      SELECT 1
      FROM account_groups ag
      JOIN groups g ON g.id = ag.group_id
      WHERE ag.account_id = a.id
        AND g.deleted_at IS NULL
        AND g.name NOT IN (
          '${SOURCE_GROUP_NAME_SQL}',
          '${HOT_GROUP_NAME_SQL}',
          '${COOLDOWN_GROUP_NAME_SQL}',
          '${DEAD_GROUP_NAME_SQL}',
          '${BUG_POOL_GROUP_NAME_SQL}',
          '${DEFAULT_GROUP_NAME_SQL}'
        )
    )
    AND NOT EXISTS (
      SELECT 1
      FROM account_groups ag
      JOIN groups g ON g.id = ag.group_id
      WHERE ag.account_id = a.id
        AND g.deleted_at IS NULL
        AND g.name IN (
          '${SOURCE_GROUP_NAME_SQL}',
          '${BUG_POOL_GROUP_NAME_SQL}'
        )
    )
),
inserted_accounts AS (
  INSERT INTO account_groups (account_id, group_id)
  SELECT c.id, sg.id
  FROM auto_adopt_candidates c
  CROSS JOIN source_group sg
  ON CONFLICT DO NOTHING
  RETURNING account_id
)
SELECT COUNT(*) FROM inserted_accounts;

WITH source_group AS (
  SELECT id
  FROM groups
  WHERE name = '${SOURCE_GROUP_NAME_SQL}'
    AND deleted_at IS NULL
),
source_group_has_members AS (
  SELECT EXISTS (
    SELECT 1
    FROM account_groups ag
    JOIN source_group sg ON sg.id = ag.group_id
  ) AS has_members
),
seed_ids AS (
  SELECT DISTINCT CAST(trim(value) AS bigint) AS account_id
  FROM regexp_split_to_table('${SOURCE_SEED_ACCOUNT_IDS_SQL}', ',') AS value
  WHERE trim(value) <> ''
),
inserted_seed AS (
  INSERT INTO account_groups (account_id, group_id)
  SELECT s.account_id, sg.id
  FROM seed_ids s
  JOIN accounts a ON a.id = s.account_id
  CROSS JOIN source_group sg
  CROSS JOIN source_group_has_members hm
  WHERE hm.has_members = false
    AND a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND NOT EXISTS (
      SELECT 1
      FROM account_groups ag
      JOIN groups g ON g.id = ag.group_id
      WHERE ag.account_id = a.id
        AND g.deleted_at IS NULL
        AND g.name NOT IN (
          '${SOURCE_GROUP_NAME_SQL}',
          '${HOT_GROUP_NAME_SQL}',
          '${COOLDOWN_GROUP_NAME_SQL}',
          '${DEAD_GROUP_NAME_SQL}',
          '${BUG_POOL_GROUP_NAME_SQL}',
          '${DEFAULT_GROUP_NAME_SQL}'
        )
    )
  ON CONFLICT DO NOTHING
  RETURNING account_id
)
SELECT COUNT(*) FROM inserted_seed;

WITH resolved_proxy AS (
  SELECT p.id
  FROM proxies p
  WHERE p.deleted_at IS NULL
    AND p.status = 'active'
    AND (
      ${DEFAULT_PROXY_ID_MATCH_SQL}
      OR p.name = '${DEFAULT_PROXY_NAME_SQL}'
      OR (
        p.host = '${DEFAULT_PROXY_HOST_SQL}'
        AND p.port = ${DEFAULT_PROXY_PORT}
        AND p.protocol = '${DEFAULT_PROXY_PROTOCOL_SQL}'
      )
    )
  ORDER BY
    CASE
${DEFAULT_PROXY_ID_ORDER_SQL}      WHEN p.name = '${DEFAULT_PROXY_NAME_SQL}' THEN 1
      WHEN p.host = '${DEFAULT_PROXY_HOST_SQL}'
        AND p.port = ${DEFAULT_PROXY_PORT}
        AND p.protocol = '${DEFAULT_PROXY_PROTOCOL_SQL}' THEN 2
      ELSE 3
    END,
    p.id
  LIMIT 1
),
source_accounts_without_proxy AS (
  SELECT DISTINCT a.id
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups sg ON sg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND a.proxy_id IS NULL
    AND sg.name = '${SOURCE_GROUP_NAME_SQL}'
    AND sg.deleted_at IS NULL
)
UPDATE accounts a
SET proxy_id = rp.id
FROM resolved_proxy rp
WHERE a.id IN (SELECT id FROM source_accounts_without_proxy)
  AND a.proxy_id IS NULL;

-- Ensure every warp-pool oauth account has gpt-5.5 in its model_mapping.
-- account.go:openAIModelRequiresExplicitMapping hard-rejects gpt-5.5 unless the
-- account explicitly maps it, so missing this key effectively benches the
-- account for gpt-5.5 traffic. Two cases:
--   1) mapping is missing/empty  -> write the full canonical template
--      (writing only {"gpt-5.5":"gpt-5.5"} would lock the account to gpt-5.5)
--   2) mapping exists without gpt-5.5 -> just merge in {"gpt-5.5":"gpt-5.5"}
WITH warp_groups AS (
  SELECT id FROM groups
  WHERE deleted_at IS NULL
    AND name IN (
      '${SOURCE_GROUP_NAME_SQL}',
      '${HOT_GROUP_NAME_SQL}',
      '${DEAD_GROUP_NAME_SQL}'
    )
),
warp_accounts_missing_mapping AS (
  SELECT DISTINCT a.id,
    COALESCE(a.credentials -> 'model_mapping', '{}'::jsonb) AS current_mapping
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN warp_groups wg ON wg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND NOT COALESCE((a.credentials -> 'model_mapping') ? 'gpt-5.5', false)
)
UPDATE accounts a
SET credentials = jsonb_set(
      COALESCE(a.credentials, '{}'::jsonb),
      '{model_mapping}',
      CASE
        WHEN t.current_mapping = '{}'::jsonb
          THEN '${CANONICAL_MODEL_MAPPING_JSON_SQL}'::jsonb
        ELSE t.current_mapping || '{"gpt-5.5":"gpt-5.5"}'::jsonb
      END,
      true
    ),
    updated_at = now()
FROM warp_accounts_missing_mapping t
WHERE a.id = t.id;

-- Ensure every warp-pool oauth account redirects the retired bare gpt-5.4 model
-- to gpt-5.5. Upstream removed gpt-5.4, and IsModelSupported (account.go) hard-
-- rejects any model whose key is absent from a non-empty mapping. Without an
-- explicit "gpt-5.4":"gpt-5.5" entry, bare gpt-5.4 traffic either fails account
-- selection (missing key) or is forwarded to the dead gpt-5.4 upstream (identity
-- key). Overwrite/insert the key for any account whose gpt-5.4 != gpt-5.5.
-- (Only touches accounts that already have a non-empty mapping; empty-mapping
-- accounts allow-all and get gpt-5.5 via the codex model normalizer.)
WITH warp_groups AS (
  SELECT id FROM groups
  WHERE deleted_at IS NULL
    AND name IN (
      '${SOURCE_GROUP_NAME_SQL}',
      '${HOT_GROUP_NAME_SQL}',
      '${DEAD_GROUP_NAME_SQL}'
    )
),
warp_accounts_bad_54 AS (
  SELECT DISTINCT a.id
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN warp_groups wg ON wg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND jsonb_typeof(a.credentials -> 'model_mapping') = 'object'
    AND a.credentials -> 'model_mapping' <> '{}'::jsonb
    AND COALESCE((a.credentials -> 'model_mapping') ->> 'gpt-5.4', '') <> 'gpt-5.5'
)
UPDATE accounts a
SET credentials = jsonb_set(a.credentials, '{model_mapping,gpt-5.4}', '"gpt-5.5"'::jsonb, true),
    updated_at = now()
FROM warp_accounts_bad_54 t
WHERE a.id = t.id;

INSERT INTO groups (
  name,
  description,
  rate_multiplier,
  is_exclusive,
  status,
  platform,
  subscription_type,
  default_validity_days,
  claude_code_only,
  model_routing,
  model_routing_enabled,
  mcp_xml_inject,
  supported_model_scopes,
  sort_order,
  allow_messages_dispatch,
  default_mapped_model,
  require_oauth_only,
  require_privacy_set,
  messages_dispatch_model_config
)
SELECT
  '${DEAD_GROUP_NAME_SQL}',
  'Auto-managed dead pool for revoked or non-active OpenAI OAuth accounts',
  g.rate_multiplier,
  g.is_exclusive,
  g.status,
  g.platform,
  g.subscription_type,
  g.default_validity_days,
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
  g.messages_dispatch_model_config
FROM groups g
WHERE g.name = '${HOT_GROUP_NAME_SQL}'
  AND g.deleted_at IS NULL
  AND NOT EXISTS (
  SELECT 1 FROM groups WHERE name = '${DEAD_GROUP_NAME_SQL}' AND deleted_at IS NULL
);

WITH warp_groups AS (
  SELECT id, name
  FROM groups
  WHERE name IN (
    '${SOURCE_GROUP_NAME_SQL}',
    '${HOT_GROUP_NAME_SQL}',
    '${COOLDOWN_GROUP_NAME_SQL}',
    '${DEAD_GROUP_NAME_SQL}'
  )
    AND deleted_at IS NULL
),
source_managed_accounts AS (
  SELECT DISTINCT ag.account_id
  FROM account_groups ag
  JOIN groups g ON g.id = ag.group_id
  WHERE g.deleted_at IS NULL
    AND g.name = '${SOURCE_GROUP_NAME_SQL}'
),
excluded_accounts AS (
  SELECT DISTINCT ag.account_id
  FROM account_groups ag
  JOIN groups g ON g.id = ag.group_id
  WHERE g.deleted_at IS NULL
    AND g.name NOT IN (
      '${SOURCE_GROUP_NAME_SQL}',
      '${HOT_GROUP_NAME_SQL}',
      '${COOLDOWN_GROUP_NAME_SQL}',
      '${DEAD_GROUP_NAME_SQL}',
      '${BUG_POOL_GROUP_NAME_SQL}',
      '${DEFAULT_GROUP_NAME_SQL}'
    )
)
DELETE FROM account_groups ag
USING warp_groups wg, excluded_accounts ea, source_managed_accounts sma
WHERE ag.group_id = wg.id
  AND ag.account_id = ea.account_id
  AND ag.account_id = sma.account_id;

WITH default_group AS (
  SELECT id
  FROM groups
  WHERE name = '${DEFAULT_GROUP_NAME_SQL}'
    AND deleted_at IS NULL
),
source_managed_accounts AS (
  SELECT DISTINCT ag.account_id
  FROM account_groups ag
  JOIN groups g ON g.id = ag.group_id
  WHERE g.deleted_at IS NULL
    AND g.name = '${SOURCE_GROUP_NAME_SQL}'
)
DELETE FROM account_groups ag
USING default_group dg, source_managed_accounts sma
WHERE ag.group_id = dg.id
  AND ag.account_id = sma.account_id
  AND NOT EXISTS (
    SELECT 1
    FROM account_groups ag2
    JOIN groups g2 ON g2.id = ag2.group_id
    WHERE ag2.account_id = ag.account_id
      AND g2.deleted_at IS NULL
      AND g2.name NOT IN (
        '${SOURCE_GROUP_NAME_SQL}',
        '${HOT_GROUP_NAME_SQL}',
        '${COOLDOWN_GROUP_NAME_SQL}',
        '${DEAD_GROUP_NAME_SQL}',
        '${BUG_POOL_GROUP_NAME_SQL}',
        '${DEFAULT_GROUP_NAME_SQL}'
      )
  );
WITH managed_groups AS (
  SELECT id, name
  FROM groups
  WHERE name IN ('${HOT_GROUP_NAME_SQL}', '${COOLDOWN_GROUP_NAME_SQL}', '${DEAD_GROUP_NAME_SQL}')
    AND deleted_at IS NULL
),
source_managed_accounts AS (
  SELECT DISTINCT ag.account_id
  FROM account_groups ag
  JOIN groups g ON g.id = ag.group_id
  WHERE g.deleted_at IS NULL
    AND g.name = '${SOURCE_GROUP_NAME_SQL}'
)
DELETE FROM account_groups ag
USING managed_groups mg, source_managed_accounts sma
WHERE ag.group_id = mg.id
  AND ag.account_id = sma.account_id;

WITH fatal_source_accounts AS (
  SELECT DISTINCT
    a.id
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups sg ON sg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND sg.name = '${SOURCE_GROUP_NAME_SQL}'
    AND sg.deleted_at IS NULL
    AND NOT EXISTS (
      SELECT 1
      FROM account_groups ag2
      JOIN groups g2 ON g2.id = ag2.group_id
      WHERE ag2.account_id = a.id
        AND g2.deleted_at IS NULL
        AND g2.name NOT IN (
          '${SOURCE_GROUP_NAME_SQL}',
          '${HOT_GROUP_NAME_SQL}',
          '${COOLDOWN_GROUP_NAME_SQL}',
          '${DEAD_GROUP_NAME_SQL}',
          '${BUG_POOL_GROUP_NAME_SQL}',
          '${DEFAULT_GROUP_NAME_SQL}'
        )
    )
    AND (
      a.status <> 'active'
      OR COALESCE(a.error_message, '') ILIKE '%token revoked%'
      OR COALESCE(a.error_message, '') ILIKE '%authentication token has been invalidated%'
      OR COALESCE(a.error_message, '') ILIKE '%access forbidden (403)%'
      OR COALESCE(a.error_message, '') ILIKE '%lack permissions%'
      OR COALESCE(a.temp_unschedulable_reason, '') ILIKE '%token revoked%'
      OR COALESCE(a.temp_unschedulable_reason, '') ILIKE '%authentication token has been invalidated%'
    )
)
UPDATE accounts a
SET schedulable = false,
    status = CASE WHEN a.status = 'active' THEN 'error' ELSE a.status END
FROM fatal_source_accounts fsa
WHERE a.id = fsa.id
  AND (a.schedulable <> false OR a.status = 'active');

WITH managed_groups AS (
  SELECT id, name
  FROM groups
  WHERE name IN ('${HOT_GROUP_NAME_SQL}', '${DEAD_GROUP_NAME_SQL}')
    AND deleted_at IS NULL
),
source_accounts AS (
  SELECT
    a.id,
    CASE
      WHEN a.status <> 'active'
        OR COALESCE(a.error_message, '') ILIKE '%token revoked%'
        OR COALESCE(a.error_message, '') ILIKE '%authentication token has been invalidated%'
        OR COALESCE(a.error_message, '') ILIKE '%access forbidden (403)%'
        OR COALESCE(a.error_message, '') ILIKE '%lack permissions%'
        OR COALESCE(a.temp_unschedulable_reason, '') ILIKE '%token revoked%'
        OR COALESCE(a.temp_unschedulable_reason, '') ILIKE '%authentication token has been invalidated%'
      THEN '${DEAD_GROUP_NAME_SQL}'
      ELSE '${HOT_GROUP_NAME_SQL}'
    END AS target_group_name
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups sg ON sg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND sg.name = '${SOURCE_GROUP_NAME_SQL}'
    AND sg.deleted_at IS NULL
    AND NOT EXISTS (
      SELECT 1
      FROM account_groups ag2
      JOIN groups g2 ON g2.id = ag2.group_id
      WHERE ag2.account_id = a.id
        AND g2.deleted_at IS NULL
        AND g2.name NOT IN (
          '${SOURCE_GROUP_NAME_SQL}',
          '${HOT_GROUP_NAME_SQL}',
          '${DEAD_GROUP_NAME_SQL}',
          '${BUG_POOL_GROUP_NAME_SQL}',
          '${DEFAULT_GROUP_NAME_SQL}'
        )
    )
)
INSERT INTO account_groups (account_id, group_id)
SELECT sa.id, mg.id
FROM source_accounts sa
JOIN managed_groups mg ON mg.name = sa.target_group_name
ON CONFLICT DO NOTHING;

WITH source_accounts AS (
  SELECT
    a.id,
    CASE
      WHEN a.status <> 'active'
        OR COALESCE(a.error_message, '') ILIKE '%token revoked%'
        OR COALESCE(a.error_message, '') ILIKE '%authentication token has been invalidated%'
        OR COALESCE(a.error_message, '') ILIKE '%access forbidden (403)%'
        OR COALESCE(a.error_message, '') ILIKE '%lack permissions%'
        OR COALESCE(a.temp_unschedulable_reason, '') ILIKE '%token revoked%'
        OR COALESCE(a.temp_unschedulable_reason, '') ILIKE '%authentication token has been invalidated%'
      THEN '${DEAD_GROUP_NAME_SQL}'
      ELSE '${HOT_GROUP_NAME_SQL}'
    END AS target_group_name
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups sg ON sg.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
    AND sg.name = '${SOURCE_GROUP_NAME_SQL}'
    AND sg.deleted_at IS NULL
    AND NOT EXISTS (
      SELECT 1
      FROM account_groups ag2
      JOIN groups g2 ON g2.id = ag2.group_id
      WHERE ag2.account_id = a.id
        AND g2.deleted_at IS NULL
        AND g2.name NOT IN (
          '${SOURCE_GROUP_NAME_SQL}',
          '${HOT_GROUP_NAME_SQL}',
          '${DEAD_GROUP_NAME_SQL}',
          '${BUG_POOL_GROUP_NAME_SQL}',
          '${DEFAULT_GROUP_NAME_SQL}'
        )
    )
)
SELECT target_group_name, COUNT(*) AS account_count
FROM source_accounts
GROUP BY target_group_name
ORDER BY target_group_name;

COMMIT;
SQL_EOF
)

echo "[$(date '+%Y-%m-%d %H:%M:%S %z')] sync start"
run_psql -P pager=off -c "$SQL"
echo "[$(date '+%Y-%m-%d %H:%M:%S %z')] sync done"
