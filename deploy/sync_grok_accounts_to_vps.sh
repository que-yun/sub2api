#!/usr/bin/env bash
set -euo pipefail
# Grok OAuth account authority sync (local -> VPS standby).
#
# Local owns account membership and scheduling configuration. VPS owns runtime
# health. New or restored credentials must pass a VPS-local active probe before
# scheduling; local health snapshots never overwrite VPS observations.
#
# Accounts absent from two consecutive successful local snapshots are soft
# deleted on the VPS. The first missing snapshot immediately removes them from
# scheduling and the Grok group, so a locally deleted account cannot continue
# serving traffic while the second confirmation is pending.

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
STANDBY_REDIS_CONTAINER="${STANDBY_REDIS_CONTAINER:-sub2api-standby-redis}"

# macOS mktemp requires the random template at the end of the path. Keeping
# the temporary dump suffix-free also avoids collisions with a literal
# `XXXXXX.tsv` file left by an older invocation.
dump_path="$(mktemp "${TMPDIR:-/tmp}/grok-accounts.XXXXXX")"
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
      && ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "test -f $(shell_quote "$2")" && return 0
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
[[ "${row_count}" == "0" ]] && echo "No active local Grok OAuth accounts in group ${GROK_GROUP_ID}; reconciling managed VPS accounts only."

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

CREATE TEMP TABLE grok_account_sync_probe_ids (
  id bigint PRIMARY KEY
);

WITH incoming AS (
  SELECT
    i.*,
    (COALESCE(i.extra, '{}'::jsonb)
      - 'grok_usage_snapshot'
      - 'grok_billing_snapshot'
      - 'grok_hold_until_success'
      - 'grok_vps_probe'
      - 'grok_vps_probe_requested_at'
      - 'grok_vps_probe_requested_revision'
      - 'grok_vps_probe_completed_at'
      - 'grok_credential_revision'
      - 'grok_error_recovery_last_probe_at'
      - 'grok_error_recovery_last_class'
      - 'grok_error_recovery_last_status'
      - 'grok_error_recovery_last_message'
      - 'grok_error_recovery_last_recovered_at'
      - 'grok_free_usage_exhausted'
      - 'grok_free_usage_error_code'
      - 'grok_free_usage_window'
      - 'grok_free_usage_model'
      - 'grok_free_usage_cooldown_until'
      - 'grok_free_usage_actual_tokens'
      - 'grok_free_usage_limit_tokens'
      - 'grok_vps_local_sync_managed'
      - 'grok_vps_local_sync_missing_since'
      - 'grok_vps_local_sync_missing_count'
      - 'grok_vps_local_sync_tombstoned_at') AS shared_extra
  FROM incoming_grok_accounts i
),
restored AS (
  UPDATE public.accounts a
  SET name = i.name,
      credentials = i.credentials,
      extra = i.shared_extra || jsonb_build_object(
        'grok_vps_local_sync_managed', true,
        'grok_credential_revision', md5(i.credentials::text),
        'grok_vps_probe_requested_at', now()::text,
        'grok_vps_probe_requested_revision', md5(i.credentials::text),
        'grok_hold_until_success', true
      ),
      concurrency = COALESCE(i.concurrency, 1),
      priority = COALESCE(i.priority, 0),
      status = 'error',
      error_message = 'grok vps credential revision pending active probe',
      deleted_at = NULL,
      schedulable = false,
      rate_limited_at = NULL,
      rate_limit_reset_at = NULL,
      overload_until = NULL,
      temp_unschedulable_until = NULL,
      temp_unschedulable_reason = NULL,
      notes = i.notes,
      expires_at = i.expires_at,
      rate_multiplier = COALESCE(i.rate_multiplier, 1),
      load_factor = i.load_factor,
      updated_at = GREATEST(a.updated_at, i.updated_at)
  FROM incoming i
  WHERE a.id = i.id
    AND a.platform = 'grok'
    AND a.type = 'oauth'
    AND (
      a.deleted_at IS NOT NULL
      OR COALESCE(a.extra, '{}'::jsonb) ? 'grok_vps_local_sync_missing_since'
    )
  RETURNING a.id
),
updated AS (
  UPDATE public.accounts a
  SET name = i.name,
      concurrency = COALESCE(i.concurrency, 1),
      priority = COALESCE(i.priority, 0),
      notes = i.notes,
      expires_at = i.expires_at,
      rate_multiplier = COALESCE(i.rate_multiplier, 1),
      load_factor = i.load_factor,
      extra = jsonb_set(
        COALESCE(a.extra, '{}'::jsonb),
        '{grok_vps_local_sync_managed}',
        'true'::jsonb,
        true
      ),
      updated_at = GREATEST(a.updated_at, i.updated_at)
  FROM incoming i
  WHERE a.id = i.id
    AND a.deleted_at IS NULL
    AND a.platform = 'grok'
    AND a.type = 'oauth'
    AND NOT (COALESCE(a.extra, '{}'::jsonb) ? 'grok_vps_local_sync_missing_since')
  RETURNING a.id
),
ins AS (
  INSERT INTO public.accounts (
    id, name, platform, type, credentials, extra, proxy_id, concurrency,
    priority, status, error_message, last_used_at, created_at, updated_at,
    deleted_at, schedulable, rate_limited_at, rate_limit_reset_at,
    overload_until, session_window_start, session_window_end,
    session_window_status, temp_unschedulable_until,
    temp_unschedulable_reason, notes, expires_at, auto_pause_on_expired,
    rate_multiplier, load_factor
  )
  SELECT i.id, i.name, 'grok', 'oauth', i.credentials,
         i.shared_extra || jsonb_build_object(
           'grok_vps_local_sync_managed', true,
           'grok_credential_revision', md5(i.credentials::text),
           'grok_vps_probe_requested_at', now()::text,
           'grok_vps_probe_requested_revision', md5(i.credentials::text),
           'grok_hold_until_success', true
         ), NULL, COALESCE(i.concurrency,1),
         COALESCE(i.priority,0), 'error', 'grok vps credential revision pending active probe', NULL, COALESCE(i.created_at,now()), COALESCE(i.updated_at,now()),
         NULL, false, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
         NULL, i.notes, i.expires_at, false, COALESCE(i.rate_multiplier,1), i.load_factor
  FROM incoming i
  WHERE NOT EXISTS (SELECT 1 FROM public.accounts a WHERE a.id = i.id)
  RETURNING id
),
probe_ids AS (
  INSERT INTO grok_account_sync_probe_ids (id)
  SELECT id FROM restored
  UNION
  SELECT id FROM ins
  ON CONFLICT (id) DO NOTHING
  RETURNING id
)
SELECT 'synced_grok_accounts inserted=' || (SELECT count(*) FROM ins)
    || ' restored=' || (SELECT count(*) FROM restored)
    || ' config_updated=' || (SELECT count(*) FROM updated)
    || ' pending_vps_active_probe=' || (SELECT count(*) FROM probe_ids);

-- Keep this separate from the data-modifying CTE above. PostgreSQL does not
-- expose rows inserted by a sibling CTE through a fresh table scan, which can
-- otherwise leave new accounts present but unbound to the Grok group.
WITH grp AS (
  INSERT INTO public.account_groups (account_id, group_id, priority, created_at)
  SELECT i.id, ${GROK_GROUP_ID}, 0, now()
  FROM incoming_grok_accounts i
  JOIN public.accounts a
    ON a.id = i.id
   AND a.deleted_at IS NULL
   AND a.platform = 'grok'
   AND a.type = 'oauth'
  ON CONFLICT (account_id, group_id) DO UPDATE SET priority = EXCLUDED.priority
  RETURNING account_id
)
SELECT 'synced_grok_account_group_links=' || count(*) FROM grp;

WITH missing AS (
  UPDATE public.accounts a
  SET status = 'error',
      schedulable = false,
      error_message = 'grok account absent from local authority snapshot',
      extra = COALESCE(a.extra, '{}'::jsonb) || jsonb_build_object(
        'grok_vps_local_sync_missing_since', COALESCE(a.extra->>'grok_vps_local_sync_missing_since', now()::text),
        'grok_vps_local_sync_missing_count', COALESCE((a.extra->>'grok_vps_local_sync_missing_count')::integer, 0) + 1
      ),
      updated_at = now()
  WHERE a.deleted_at IS NULL
    AND a.platform = 'grok'
    AND a.type = 'oauth'
    AND COALESCE(a.extra->>'grok_vps_local_sync_managed', 'false') = 'true'
    AND NOT EXISTS (SELECT 1 FROM incoming_grok_accounts i WHERE i.id = a.id)
  RETURNING a.id
)
SELECT 'quarantined_missing_grok_accounts=' || count(*) FROM missing;

DELETE FROM public.account_groups ag
USING public.accounts a
WHERE ag.account_id = a.id
  AND ag.group_id = ${GROK_GROUP_ID}
  AND a.deleted_at IS NULL
  AND a.platform = 'grok'
  AND a.type = 'oauth'
  AND COALESCE(a.extra->>'grok_vps_local_sync_managed', 'false') = 'true'
  AND a.extra ? 'grok_vps_local_sync_missing_since'
  AND NOT EXISTS (SELECT 1 FROM incoming_grok_accounts i WHERE i.id = a.id);

WITH tombstoned AS (
  UPDATE public.accounts a
  SET deleted_at = now(),
      schedulable = false,
      status = 'error',
      error_message = 'grok account absent from local authority snapshot for two sync cycles',
      extra = jsonb_set(
        COALESCE(a.extra, '{}'::jsonb),
        '{grok_vps_local_sync_tombstoned_at}',
        to_jsonb(now()::text),
        true
      ),
      updated_at = now()
  WHERE a.deleted_at IS NULL
    AND a.platform = 'grok'
    AND a.type = 'oauth'
    AND COALESCE(a.extra->>'grok_vps_local_sync_managed', 'false') = 'true'
    AND COALESCE((a.extra->>'grok_vps_local_sync_missing_count')::integer, 0) >= 2
    AND NOT EXISTS (SELECT 1 FROM incoming_grok_accounts i WHERE i.id = a.id)
  RETURNING a.id
)
SELECT 'soft_deleted_missing_grok_accounts=' || count(*) FROM tombstoned;

\copy (SELECT id FROM grok_account_sync_probe_ids ORDER BY id) TO '/tmp/grok-account-sync-probe-ids.tsv' WITH (FORMAT csv, DELIMITER E'\t', HEADER false);

SELECT setval(pg_get_serial_sequence('public.accounts','id'),
       GREATEST((SELECT COALESCE(MAX(id),1) FROM public.accounts),1), true);
SQL
docker exec ${STANDBY_PG_CONTAINER} rm -f /tmp/grok-accounts.tsv
rm -f $(shell_quote "${remote_path}")
if docker exec ${STANDBY_PG_CONTAINER} test -s /tmp/grok-account-sync-probe-ids.tsv 2>/dev/null; then
  docker cp ${STANDBY_PG_CONTAINER}:/tmp/grok-account-sync-probe-ids.tsv /tmp/grok-account-sync-probe-ids.tsv
  probe_count=0
  while IFS= read -r account_id; do
    [ -z "\${account_id}" ] && continue
    docker exec $(shell_quote "${STANDBY_REDIS_CONTAINER}") env -u REDISCLI_AUTH redis-cli DEL \
      "oauth:token:grok:account:\${account_id}" \
      "oauth:refresh_lock:grok:account:\${account_id}" >/dev/null || true
    probe_count=\$((probe_count + 1))
  done < /tmp/grok-account-sync-probe-ids.tsv
  echo "Invalidated VPS Grok token cache entries for new/restored accounts: \${probe_count}"
  rm -f /tmp/grok-account-sync-probe-ids.tsv
fi
docker exec ${STANDBY_PG_CONTAINER} rm -f /tmp/grok-account-sync-probe-ids.tsv
"
echo "grok accounts sync done."
