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
STANDBY_REDIS_CONTAINER="${STANDBY_REDIS_CONTAINER:-sub2api-standby-redis}"
STANDBY_IGNORE_PROXY_IDS="${STANDBY_IGNORE_PROXY_IDS:-6}"
STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS="${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS:-false}"
STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
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

SSH_RETRIES="${SSH_RETRIES:-4}"
SSH_RETRY_SLEEP_SECONDS="${SSH_RETRY_SLEEP_SECONDS:-8}"

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
# 必须用 docker exec -i：不加 -i 时 heredoc 不会喂进容器内 psql 的 stdin，凭证 UPDATE 会静默空转
# （其它增量脚本 scheduling/anthropic/route_config 都带 -i，唯独这里漏了，导致 openai token 增量长期无效）。
docker exec -i sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE local_openai_oauth_credentials (
  id bigint PRIMARY KEY,
  credentials jsonb NOT NULL,
  updated_at timestamptz NOT NULL
);

\copy local_openai_oauth_credentials (id, credentials, updated_at) FROM '/tmp/openai-oauth-credentials.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

CREATE TEMP TABLE openai_oauth_credentials_changed_ids (
  id bigint PRIMARY KEY
);

WITH changed AS (
  SELECT
    a.id,
    l.credentials AS new_credentials,
    l.updated_at AS local_updated_at,
    (a.credentials IS DISTINCT FROM l.credentials) AS credentials_changed
  FROM public.accounts a
  JOIN local_openai_oauth_credentials l ON a.id = l.id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'openai'
    AND a.type = 'oauth'
),
updated AS (
  UPDATE public.accounts a
  SET credentials = c.new_credentials,
      updated_at = GREATEST(a.updated_at, c.local_updated_at)
      -- 只推凭证，不因 credentials 变化改 status/schedulable/error。
      -- 本机探测/恢复后由 scheduling sync 推状态；避免 VPS 假 active。
  FROM changed c
  WHERE a.id = c.id
  RETURNING a.id, c.credentials_changed
),
ins AS (
  INSERT INTO openai_oauth_credentials_changed_ids (id)
  SELECT id FROM updated WHERE credentials_changed
  RETURNING id
),
stats AS (
  SELECT
    count(*) AS synced,
    count(*) FILTER (WHERE credentials_changed) AS credentials_changed
  FROM updated
)
SELECT
  'synced_openai_oauth_accounts=' || synced ||
  ' credentials_changed=' || credentials_changed
FROM stats;

\copy (SELECT id FROM openai_oauth_credentials_changed_ids ORDER BY id) TO '/tmp/openai-oauth-credentials-changed-ids.tsv' WITH (FORMAT csv, DELIMITER E'\t', HEADER false);
SQL
docker exec sub2api-standby-postgres rm -f /tmp/openai-oauth-credentials.tsv
rm -f $(shell_quote "${remote_path}")
# 推新 credentials 后失效 VPS 本机 Redis 旧 AT。
if docker exec sub2api-standby-postgres test -s /tmp/openai-oauth-credentials-changed-ids.tsv 2>/dev/null; then
  docker cp sub2api-standby-postgres:/tmp/openai-oauth-credentials-changed-ids.tsv /tmp/openai-oauth-credentials-changed-ids.tsv
  changed_count=0
  while IFS= read -r account_id; do
    [ -z "\${account_id}" ] && continue
    docker exec $(shell_quote "${STANDBY_REDIS_CONTAINER}") env -u REDISCLI_AUTH redis-cli DEL \
      "oauth:token:openai:account:\${account_id}" \
      "oauth:refresh_lock:openai:account:\${account_id}" >/dev/null || true
    changed_count=\$((changed_count + 1))
  done < /tmp/openai-oauth-credentials-changed-ids.tsv
  echo "Invalidated VPS OpenAI token cache entries: \${changed_count}"
  rm -f /tmp/openai-oauth-credentials-changed-ids.tsv
  docker exec sub2api-standby-postgres rm -f /tmp/openai-oauth-credentials-changed-ids.tsv
fi
if [[ $(shell_quote "${STANDBY_PROXY_POLICY}") == "remap_local" ]]; then
  docker exec sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
INSERT INTO public.proxies (id, name, protocol, host, port, status, created_at, updated_at, fallback_mode, expiry_warn_days)
VALUES (
  ${STANDBY_LOCAL_PROXY_ID},
  'vps-mihomo-socks5-7891',
  'socks5',
  '127.0.0.1',
  7891,
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
WITH remapped AS (
  UPDATE public.accounts
  SET proxy_id = ${STANDBY_LOCAL_PROXY_ID}, updated_at = now()
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'oauth'
    AND proxy_id IS NOT NULL
    AND proxy_id IS DISTINCT FROM ${STANDBY_LOCAL_PROXY_ID}
  RETURNING id
)
SELECT 'standby_remapped_openai_oauth_proxy_accounts=' || count(*) FROM remapped;\"
elif [[ $(shell_quote "${STANDBY_CLEAR_OPENAI_OAUTH_PROXY_IDS}") == true || $(shell_quote "${STANDBY_PROXY_POLICY}") == "clear_all" ]]; then
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
if grep -q '^TOKEN_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
  sed -i 's/^TOKEN_REFRESH_ENABLED=.*/TOKEN_REFRESH_ENABLED=false/' /etc/sub2api-standby.env
else
  printf '\nTOKEN_REFRESH_ENABLED=false\n' >> /etc/sub2api-standby.env
fi
if grep -q '^TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
  sed -i 's/^TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_REQUEST_REFRESH_ENABLED=false/' /etc/sub2api-standby.env
else
  printf '\nTOKEN_REFRESH_REQUEST_REFRESH_ENABLED=false\n' >> /etc/sub2api-standby.env
fi
# 兼容旧 env 名；真正生效的是全局 request_refresh_enabled。
if grep -q '^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=' /etc/sub2api-standby.env; then
  sed -i 's/^TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=.*/TOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=false/' /etc/sub2api-standby.env
else
  printf '\nTOKEN_REFRESH_OPENAI_REQUEST_REFRESH_ENABLED=false\n' >> /etc/sub2api-standby.env
fi
if [[ $(shell_quote "${STANDBY_RESTART_AFTER_TOKEN_SYNC}") == true ]]; then
  systemctl restart $(shell_quote "${SERVICE_NAME}")
fi"

echo "OpenAI OAuth token sync completed."
