#!/usr/bin/env bash
set -euo pipefail

# grok OAuth 凭证增量同步（local -> VPS standby）。
# 目的：把本地 remint/refresh 后的最新 grok 凭证原地 UPDATE 到 VPS 正在服务的库，
#      不做 dump/restore、不 shadow_swap、不重启 sub2api、不触碰 usage/日志表。
# 这样 grok 的新 token 能在 10 分钟内到达 VPS，而不必等每日那次全量 warm-standby。
# 本脚本只推 credentials；当 credentials 实际变化时顺带恢复 active/schedulable 并清冷却，
# 因为 VPS 过期只做临时冷却，等本机同步后应立刻可调度（不做 VPS 侧 refresh）。
# 结构与用法与 sync_openai_oauth_tokens_to_vps.sh 对齐，便于维护。

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

tmp_dir="$(mktemp -d)"
dump_path="${tmp_dir}/grok-oauth-credentials.tsv"
remote_path="/tmp/sub2api-grok-oauth-credentials.tsv"

cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

shell_quote() {
  printf "%q" "$1"
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
  echo "Exporting local Grok OAuth credentials from ${LOCAL_PG_CONTAINER} ..."
else
  echo "Exporting local Grok OAuth credentials from ${LOCAL_PG_HOST}:${LOCAL_PG_PORT}/${PGDATABASE} ..."
fi
local_psql -v ON_ERROR_STOP=1 -c "
COPY (
  SELECT
    id,
    credentials,
    updated_at
  FROM public.accounts
  WHERE deleted_at IS NULL
    AND platform = 'grok'
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

echo "Uploading Grok OAuth credentials to VPS ..."
remote_copy "${dump_path}" "${remote_path}"

remote_exec "set -euo pipefail
docker cp $(shell_quote "${remote_path}") sub2api-standby-postgres:/tmp/grok-oauth-credentials.tsv
# 必须用 docker exec -i：不加 -i 时 heredoc 不会喂进容器内 psql 的 stdin，UPDATE 会静默空转。
docker exec -i sub2api-standby-postgres psql -U $(shell_quote "${PGUSER}") -d $(shell_quote "${PGDATABASE}") -v ON_ERROR_STOP=1 <<'SQL'
CREATE TEMP TABLE local_grok_oauth_credentials (
  id bigint PRIMARY KEY,
  credentials jsonb NOT NULL,
  updated_at timestamptz NOT NULL
);

\copy local_grok_oauth_credentials (id, credentials, updated_at) FROM '/tmp/grok-oauth-credentials.tsv' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

CREATE TEMP TABLE grok_oauth_credentials_changed_ids (
  id bigint PRIMARY KEY
);

WITH changed AS (
  SELECT
    a.id,
    l.credentials AS new_credentials,
    l.updated_at AS local_updated_at,
    (a.credentials IS DISTINCT FROM l.credentials) AS credentials_changed
  FROM public.accounts a
  JOIN local_grok_oauth_credentials l ON a.id = l.id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'grok'
    AND a.type = 'oauth'
),
updated AS (
  UPDATE public.accounts a
  SET credentials = c.new_credentials,
      updated_at = GREATEST(a.updated_at, c.local_updated_at),
      -- 仅当凭证内容真变化时恢复调度态，避免覆盖 VPS 本地临时冷却/业务错误。
      status = CASE WHEN c.credentials_changed THEN 'active' ELSE a.status END,
      schedulable = CASE WHEN c.credentials_changed THEN true ELSE a.schedulable END,
      error_message = CASE WHEN c.credentials_changed THEN NULL ELSE a.error_message END,
      temp_unschedulable_until = CASE WHEN c.credentials_changed THEN NULL ELSE a.temp_unschedulable_until END,
      temp_unschedulable_reason = CASE WHEN c.credentials_changed THEN NULL ELSE a.temp_unschedulable_reason END,
      -- 新 token 到达即"重新给一次机会"：清掉 403 权限封禁遗留的 grok_hold_until_success。
      -- 否则上面的 status=active/schedulable=true 会造成 hold=true 却可调度的僵尸态，污染选号 TopK
      -- (调度器 meta 已能识别 hold 并排除，但僵尸态本身仍是脏数据、仪表盘误显示可用)。清 hold 后账号
      -- 获得一次真实 chat 尝试：能用则夺回；仍被拒则由转发层 403 正常重新 hold(status=error/schedulable=false)，
      -- 比僵尸态干净。恢复服务只做 billing 探测、不发真实 chat，held 号靠它证明不了成功，故用新 token 触发重试。
      extra = CASE WHEN c.credentials_changed AND a.extra ? 'grok_hold_until_success'
                   THEN a.extra - 'grok_hold_until_success' ELSE a.extra END
  FROM changed c
  WHERE a.id = c.id
  RETURNING a.id, c.credentials_changed
),
ins AS (
  INSERT INTO grok_oauth_credentials_changed_ids (id)
  SELECT id FROM updated WHERE credentials_changed
  RETURNING id
),
stats AS (
  SELECT
    count(*) AS synced,
    count(*) FILTER (WHERE credentials_changed) AS healed
  FROM updated
)
SELECT
  'synced_grok_oauth_accounts=' || synced ||
  ' healed_status_on_credential_change=' || healed
FROM stats;

\copy (SELECT id FROM grok_oauth_credentials_changed_ids ORDER BY id) TO '/tmp/grok-oauth-credentials-changed-ids.tsv' WITH (FORMAT csv, DELIMITER E'\t', HEADER false);
SQL
docker exec sub2api-standby-postgres rm -f /tmp/grok-oauth-credentials.tsv
rm -f $(shell_quote "${remote_path}")
# 推新 credentials 后失效 VPS 本机 Redis 旧 AT（各机 Redis 私有，不跨机同步）。
if docker exec sub2api-standby-postgres test -s /tmp/grok-oauth-credentials-changed-ids.tsv 2>/dev/null; then
  docker cp sub2api-standby-postgres:/tmp/grok-oauth-credentials-changed-ids.tsv /tmp/grok-oauth-credentials-changed-ids.tsv
  changed_count=0
  while IFS= read -r account_id; do
    [ -z "\${account_id}" ] && continue
    docker exec $(shell_quote "${STANDBY_REDIS_CONTAINER}") env -u REDISCLI_AUTH redis-cli DEL \
      "oauth:token:grok:account:\${account_id}" \
      "oauth:refresh_lock:grok:account:\${account_id}" >/dev/null || true
    changed_count=\$((changed_count + 1))
  done < /tmp/grok-oauth-credentials-changed-ids.tsv
  echo "Invalidated VPS Grok token cache entries: \${changed_count}"
  rm -f /tmp/grok-oauth-credentials-changed-ids.tsv
  docker exec sub2api-standby-postgres rm -f /tmp/grok-oauth-credentials-changed-ids.tsv
fi
"

# 保证 VPS standby 不在请求路径刷新 Grok/通用 OAuth access_token（本机权威）。
remote_exec "set -euo pipefail
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
"

echo "Grok OAuth token sync completed."
