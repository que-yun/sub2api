#!/usr/bin/env bash
set -euo pipefail
# grok 账号「建号」增量同步（local -> VPS standby）。
# 目的：把本地新注册、VPS 上还没有的 grok 号 INSERT 到 VPS，让新号真正进 VPS 号池。
# 与 sync_grok_oauth_tokens_to_vps.sh 分工：那个只 UPDATE 已有号的凭证，本脚本只负责建新号。
# 安全边界：仅 INSERT ... WHERE NOT EXISTS + account_groups(44) ON CONFLICT DO NOTHING；
#   绝不 DELETE、不改 groups/api_keys/已存在号；idempotent，可安全定时重跑。

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${SCRIPT_DIR}/host-run.env}"
if [[ -f "${ACTIVE_HOST_ENV}" ]]; then set -a; source "${ACTIVE_HOST_ENV}"; set +a; fi

export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
REMOTE_COPY_HOST="${REMOTE_COPY_HOST:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-22222}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-${HOME}/.ssh/sub2api_vps_db_tunnel_ed25519}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-15}"
SSH_RETRIES="${SSH_RETRIES:-4}"
SSH_RETRY_SLEEP_SECONDS="${SSH_RETRY_SLEEP_SECONDS:-8}"

LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5433}}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-}}"
PGUSER="${PGUSER:-${DATABASE_USER:-sub2api}}"
PGDATABASE="${PGDATABASE:-${DATABASE_DBNAME:-sub2api}}"
GROK_GROUP_ID="${GROK_GROUP_ID:-44}"
STANDBY_PG_CONTAINER="${STANDBY_PG_CONTAINER:-sub2api-standby-postgres}"

dump_path="$(mktemp /tmp/grok-accounts-XXXXXX.tsv)"
remote_path="/tmp/sub2api-grok-accounts.tsv"
trap 'rm -f "${dump_path}"' EXIT

shell_quote() { printf '%q' "$1"; }
ssh_args=(-o BatchMode=yes -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}" -o StrictHostKeyChecking=accept-new)
[[ -n "${SSH_PORT}" ]] && ssh_args+=(-p "${SSH_PORT}")
[[ -n "${SSH_IDENTITY_FILE}" ]] && ssh_args+=(-i "${SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes)

remote_exec() {
  local attempt=1 rc=0
  while true; do
    ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" "$@" && return 0
    rc=$?; (( attempt >= SSH_RETRIES )) && return "${rc}"
    echo "remote_exec failed (exit=${rc}), retry ${attempt}/${SSH_RETRIES} in ${SSH_RETRY_SLEEP_SECONDS}s ..." >&2
    sleep "${SSH_RETRY_SLEEP_SECONDS}"; attempt=$((attempt + 1))
  done
}
remote_copy() {
  local attempt=1 rc=0
  while true; do
    gzip -c "$1" | ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "gzip -dc > $(shell_quote "$2")" \
      && ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "test -s $(shell_quote "$2")" && return 0
    rc=$?; (( attempt >= SSH_RETRIES )) && return "${rc}"
    echo "remote_copy failed (exit=${rc}), retry ${attempt}/${SSH_RETRY_SLEEP_SECONDS}s ..." >&2
    sleep "${SSH_RETRY_SLEEP_SECONDS}"; attempt=$((attempt + 1))
  done
}
local_psql() {
  [[ -z "${LOCAL_PG_PASSWORD}" ]] && { echo "LOCAL_PG_PASSWORD/DATABASE_PASSWORD required" >&2; exit 1; }
  PGPASSWORD="${LOCAL_PG_PASSWORD}" psql -h "${LOCAL_PG_HOST}" -p "${LOCAL_PG_PORT}" -U "${PGUSER}" -d "${PGDATABASE}" "$@"
}

echo "Exporting local grok accounts (group ${GROK_GROUP_ID}) from ${LOCAL_PG_HOST}:${LOCAL_PG_PORT}/${PGDATABASE} ..."
local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT a.id, a.name, a.credentials, COALESCE(a.extra,'{}'::jsonb),
         COALESCE(a.concurrency,1), COALESCE(a.priority,0),
         a.notes, a.expires_at, COALESCE(a.rate_multiplier,1), a.load_factor,
         COALESCE(a.created_at, now()), COALESCE(a.updated_at, now())
  FROM public.accounts a
  JOIN public.account_groups ag ON ag.account_id = a.id AND ag.group_id = ${GROK_GROUP_ID}
  WHERE a.deleted_at IS NULL AND a.platform = 'grok' AND a.type = 'oauth'
    AND a.credentials ? 'refresh_token' AND a.credentials ? 'access_token' AND a.credentials ? 'expires_at'
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${dump_path}"
row_count="$(wc -l < "${dump_path}" | tr -d ' ')"
echo "Exported rows: ${row_count}"
[[ "${row_count}" == "0" ]] && { echo "nothing to sync"; exit 0; }

echo "Uploading to VPS ..."
remote_copy "${dump_path}" "${remote_path}"

remote_exec "set -euo pipefail
docker cp $(shell_quote "${remote_path}") ${STANDBY_PG_CONTAINER}:/tmp/grok-accounts.tsv
docker exec -i ${STANDBY_PG_CONTAINER} psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE incoming_grok_accounts (
  id bigint PRIMARY KEY, name text, credentials jsonb, extra jsonb,
  concurrency int, priority int, notes text, expires_at timestamptz,
  rate_multiplier numeric, load_factor numeric,
  created_at timestamptz, updated_at timestamptz
);
\copy incoming_grok_accounts (id,name,credentials,extra,concurrency,priority,notes,expires_at,rate_multiplier,load_factor,created_at,updated_at) FROM '/tmp/grok-accounts.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH ins AS (
  INSERT INTO public.accounts (
    id, name, platform, type, credentials, extra, proxy_id, concurrency,
    priority, status, error_message, last_used_at, created_at, updated_at,
    deleted_at, schedulable, rate_limited_at, rate_limit_reset_at,
    overload_until, session_window_start, session_window_end,
    session_window_status, temp_unschedulable_until,
    temp_unschedulable_reason, notes, expires_at, auto_pause_on_expired,
    rate_multiplier, load_factor
  )
  SELECT i.id, i.name, 'grok', 'oauth', i.credentials, COALESCE(i.extra,'{}'::jsonb), NULL, COALESCE(i.concurrency,1),
         COALESCE(i.priority,0), 'active', NULL, NULL, COALESCE(i.created_at,now()), COALESCE(i.updated_at,now()),
         NULL, true, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
         NULL, i.notes, i.expires_at, false, COALESCE(i.rate_multiplier,1), i.load_factor
  FROM incoming_grok_accounts i
  WHERE NOT EXISTS (SELECT 1 FROM public.accounts a WHERE a.id = i.id)
  RETURNING id
),
grp AS (
  INSERT INTO public.account_groups (account_id, group_id, priority, created_at)
  SELECT id, ${GROK_GROUP_ID}, 0, now() FROM ins
  ON CONFLICT (account_id, group_id) DO NOTHING
  RETURNING account_id
)
SELECT 'inserted_grok_accounts=' || (SELECT count(*) FROM ins)
    || ' group_links_added=' || (SELECT count(*) FROM grp);

SELECT setval(pg_get_serial_sequence('public.accounts','id'),
       GREATEST((SELECT COALESCE(MAX(id),1) FROM public.accounts),1), true);
SQL
"
echo "grok accounts sync done."
