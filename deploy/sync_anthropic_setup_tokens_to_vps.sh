#!/usr/bin/env bash
set -euo pipefail

# One-way sync: local anthropic setup-token credentials -> VPS.
# Does NOT delete VPS-only accounts. Local is token source of truth.

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
REMOTE_COPY_HOST="${REMOTE_COPY_HOST:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-15}"
LOCAL_PG_CONTAINER="${LOCAL_PG_CONTAINER:-sub2api-postgres}"
LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-127.0.0.1}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-5432}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${PGPASSWORD:-}}"
PGUSER="${PGUSER:-sub2api}"
PGDATABASE="${PGDATABASE:-sub2api}"
STANDBY_RESTART_AFTER_TOKEN_SYNC="${STANDBY_RESTART_AFTER_TOKEN_SYNC:-false}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"
# If true, clear error/temp unschedulable and force active/schedulable when local is active.
RECOVER_REMOTE_STATUS="${RECOVER_REMOTE_STATUS:-true}"

tmp_dir="$(mktemp -d)"
dump_path="${tmp_dir}/anthropic-setup-tokens.tsv"
remote_path="/tmp/sub2api-anthropic-setup-tokens.tsv"

cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

shell_quote() {
  printf "%q" "$1"
}

ssh_args=(
  -o BatchMode=yes
  -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}"
  -o ServerAliveInterval=10
  -o ServerAliveCountMax=3
  -o StrictHostKeyChecking=accept-new
)
if [[ -n "${SSH_PORT}" ]]; then
  ssh_args+=(-p "${SSH_PORT}")
fi
if [[ -n "${SSH_IDENTITY_FILE}" ]]; then
  ssh_args+=(-i "${SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes)
fi

remote_exec() {
  ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" "$@"
}

remote_copy() {
  gzip -c "$1" | ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "gzip -dc > $(shell_quote "$2")"
  ssh "${ssh_args[@]}" "${REMOTE_COPY_HOST}" "test -s $(shell_quote "$2")"
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

echo "Exporting local anthropic setup-token credentials ..."
local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    a.id,
    a.name,
    a.platform,
    a.type,
    a.credentials,
    COALESCE(a.extra, '{}'::jsonb) AS extra,
    a.priority,
    a.status,
    a.schedulable,
    a.concurrency,
    a.updated_at,
    COALESCE((
      SELECT string_agg(g.name, ',' ORDER BY g.name)
      FROM account_groups ag
      JOIN groups g ON g.id = ag.group_id AND g.deleted_at IS NULL
      WHERE ag.account_id = a.id
    ), '') AS group_names
  FROM public.accounts a
  WHERE a.deleted_at IS NULL
    AND a.platform = 'anthropic'
    AND a.type = 'setup-token'
    AND a.credentials ? 'access_token'
    AND a.credentials ? 'refresh_token'
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${dump_path}"

row_count="$(wc -l < "${dump_path}" | tr -d ' ')"
echo "Exported anthropic setup-token rows: ${row_count}"
if [[ "${row_count}" == "0" ]]; then
  echo "No anthropic setup-token accounts to sync."
  exit 0
fi

echo "Uploading anthropic setup-token credentials to VPS ..."
remote_copy "${dump_path}" "${remote_path}"

recover_flag="${RECOVER_REMOTE_STATUS}"
pguser_q="$(shell_quote "${PGUSER}")"
pgdb_q="$(shell_quote "${PGDATABASE}")"
remote_path_q="$(shell_quote "${remote_path}")"
service_q="$(shell_quote "${SERVICE_NAME}")"
restart_flag="${STANDBY_RESTART_AFTER_TOKEN_SYNC}"

remote_exec "set -euo pipefail
docker cp ${remote_path_q} sub2api-standby-postgres:/tmp/anthropic-setup-tokens.tsv
docker exec -i sub2api-standby-postgres psql -U ${pguser_q} -d ${pgdb_q} -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE local_anthropic_setup_tokens (
  id bigint PRIMARY KEY,
  name text NOT NULL,
  platform text NOT NULL,
  type text NOT NULL,
  credentials jsonb NOT NULL,
  extra jsonb NOT NULL,
  priority integer NOT NULL,
  status text NOT NULL,
  schedulable boolean NOT NULL,
  concurrency integer,
  updated_at timestamptz NOT NULL,
  group_names text NOT NULL
);

\\copy local_anthropic_setup_tokens (id, name, platform, type, credentials, extra, priority, status, schedulable, concurrency, updated_at, group_names) FROM '/tmp/anthropic-setup-tokens.tsv' WITH (FORMAT csv, DELIMITER E'\\t', QUOTE E'\\b');

WITH updated AS (
  UPDATE public.accounts a
  SET credentials = l.credentials,
      extra = COALESCE(a.extra, '{}'::jsonb) || COALESCE(l.extra, '{}'::jsonb),
      priority = l.priority,
      concurrency = COALESCE(l.concurrency, a.concurrency),
      status = CASE
        WHEN '${recover_flag}' = 'true' AND l.status = 'active' THEN 'active'
        ELSE a.status
      END,
      schedulable = CASE
        WHEN '${recover_flag}' = 'true' AND l.schedulable THEN true
        ELSE a.schedulable
      END,
      error_message = CASE
        WHEN '${recover_flag}' = 'true' AND l.status = 'active' THEN NULL
        ELSE a.error_message
      END,
      temp_unschedulable_until = CASE
        WHEN '${recover_flag}' = 'true' AND l.status = 'active' THEN NULL
        ELSE a.temp_unschedulable_until
      END,
      temp_unschedulable_reason = CASE
        WHEN '${recover_flag}' = 'true' AND l.status = 'active' THEN NULL
        ELSE a.temp_unschedulable_reason
      END,
      updated_at = GREATEST(a.updated_at, l.updated_at, now())
  FROM local_anthropic_setup_tokens l
  WHERE a.id = l.id
    AND a.deleted_at IS NULL
    AND a.platform = 'anthropic'
    AND a.type = 'setup-token'
  RETURNING a.id
),
inserted AS (
  INSERT INTO public.accounts (
    id, name, platform, type, credentials, extra, proxy_id, concurrency,
    priority, status, schedulable, created_at, updated_at
  )
  SELECT
    l.id, l.name, l.platform, l.type, l.credentials, l.extra, NULL,
    COALESCE(l.concurrency, 1), l.priority, l.status, l.schedulable,
    now(), l.updated_at
  FROM local_anthropic_setup_tokens l
  WHERE NOT EXISTS (
    SELECT 1 FROM public.accounts a WHERE a.id = l.id AND a.deleted_at IS NULL
  )
  RETURNING id
),
outbox AS (
  INSERT INTO public.scheduler_outbox (event_type, account_id, group_id, payload)
  SELECT 'account_changed', x.id, NULL, '{}'::jsonb
  FROM (
    SELECT id FROM updated
    UNION
    SELECT id FROM inserted
  ) x
  RETURNING id
)
SELECT
  'anthropic_setup_token_sync updated=' || (SELECT count(*) FROM updated) ||
  ' inserted=' || (SELECT count(*) FROM inserted) ||
  ' outbox=' || (SELECT count(*) FROM outbox);

INSERT INTO public.account_groups (account_id, group_id, priority, created_at)
SELECT l.id, g.id, 0, now()
FROM local_anthropic_setup_tokens l
CROSS JOIN LATERAL unnest(string_to_array(l.group_names, ',')) AS gn(name)
JOIN public.groups g ON g.name = trim(gn.name) AND g.deleted_at IS NULL
WHERE trim(gn.name) <> ''
ON CONFLICT (account_id, group_id) DO NOTHING;
SQL
docker exec sub2api-standby-postgres rm -f /tmp/anthropic-setup-tokens.tsv
rm -f ${remote_path_q}
if [[ ${restart_flag} == true ]]; then
  systemctl restart ${service_q}
fi"

echo "Anthropic setup-token sync completed."
