#!/usr/bin/env bash
set -euo pipefail

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
STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-6}"
STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-true}"
STANDBY_RESTART_AFTER_TOKEN_SYNC="${STANDBY_RESTART_AFTER_TOKEN_SYNC:-false}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"

tmp_dir="$(mktemp -d)"
dump_path="${tmp_dir}/openai-oauth-credentials.tsv"
remote_path="/tmp/sub2api-openai-oauth-credentials.tsv"

cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

shell_quote() {
  printf "%q" "$1"
}

sql_literal() {
  printf "'%s'" "${1//\'/\'\'}"
}

ssh_args=(
  -o BatchMode=yes
  -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}"
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

if [[ "${LOCAL_PG_SOURCE}" == "docker" ]]; then
  echo "Exporting local OpenAI OAuth credentials from ${LOCAL_PG_CONTAINER} ..."
else
  echo "Exporting local OpenAI OAuth credentials from ${LOCAL_PG_HOST}:${LOCAL_PG_PORT}/${PGDATABASE} ..."
fi
local_psql -v ON_ERROR_STOP=1 -c "
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
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');
" > "${dump_path}"

row_count="$(wc -l < "${dump_path}" | tr -d ' ')"
echo "Exported rows: ${row_count}"
if [[ "${row_count}" == "0" ]]; then
  exit 0
fi

echo "Uploading OpenAI OAuth credentials to VPS ..."
remote_copy "${dump_path}" "${remote_path}"

ignore_proxy_ids_sql="$(sql_literal "${STANDBY_IGNORE_PROXY_IDS}")"
remote_exec "set -euo pipefail
docker cp $(shell_quote "${remote_path}") sub2api-standby-postgres:/tmp/openai-oauth-credentials.tsv
docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE local_openai_oauth_credentials (
  id bigint PRIMARY KEY,
  credentials jsonb NOT NULL,
  updated_at timestamptz NOT NULL
);

\copy local_openai_oauth_credentials (id, credentials, updated_at) FROM '/tmp/openai-oauth-credentials.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH updated AS (
  UPDATE public.accounts a
  SET credentials = l.credentials,
      updated_at = GREATEST(a.updated_at, l.updated_at)
  FROM local_openai_oauth_credentials l
  WHERE a.id = l.id
    AND a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
  RETURNING a.id
)
SELECT 'synced_openai_oauth_accounts=' || count(*) FROM updated;
SQL
docker exec sub2api-standby-postgres rm -f /tmp/openai-oauth-credentials.tsv
rm -f $(shell_quote "${remote_path}")
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
if grep -q '^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
  sed -i 's/^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=false/' /etc/sub2api-standby.env
else
  printf '\nTOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=false\n' >> /etc/sub2api-standby.env
fi
if grep -q '^TOKEN_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
  sed -i 's/^TOKEN_REFRESH_ENABLED=.*/TOKEN_REFRESH_ENABLED=false/' /etc/sub2api-standby.env
else
  printf '\nTOKEN_REFRESH_ENABLED=false\n' >> /etc/sub2api-standby.env
fi
if [[ $(shell_quote "${STANDBY_RESTART_AFTER_TOKEN_SYNC}") == true ]]; then
  systemctl restart $(shell_quote "${SERVICE_NAME}")
fi"

echo "OpenAI OAuth token sync completed."
