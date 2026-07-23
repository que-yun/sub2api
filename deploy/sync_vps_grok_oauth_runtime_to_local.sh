#!/usr/bin/env bash
set -euo pipefail

# 从 VPS 回拉 Grok OAuth 的节点观测，不合并运行时健康状态。
#
# 本机拥有账号凭据与本机调度状态；VPS 只报告自身出口上的 active probe
# 失败。这样 VPS 的 401/402/403 会触发本机复探，但绝不直接改写本机
# status、schedulable、冷却或 hold。429 是节点级冷却，也不跨机器同步。

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
tmp_dir="$(mktemp -d)"
runtime_path="${tmp_dir}/vps-grok-oauth-runtime.tsv"

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


echo "Pulling VPS Grok OAuth active-probe failure observations ..."
# 只传 VPS 自己主动探测过的 401/402/403。远端凭据指纹和本机匹配后，
# 本机才会接收该观测，避免旧凭据的失败污染新凭据。
remote_exec "timeout $(printf "%q" "${REMOTE_QUERY_TIMEOUT_SECONDS}") docker exec sub2api-standby-postgres psql -U $(printf "%q" "${PGUSER}") -d $(printf "%q" "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    id,
    md5(credentials::text) AS credential_revision,
    status,
    schedulable,
    extra->'grok_usage_snapshot'->>'status_code' AS status_code,
    extra->'grok_usage_snapshot'->>'observation_source' AS observation_source,
    extra->'grok_usage_snapshot'->>'last_probe_at' AS last_probe_at,
    updated_at AS vps_updated_at
  FROM public.accounts
  WHERE deleted_at IS NULL
    AND platform = 'grok'
    AND type = 'oauth'
    AND COALESCE(extra->'grok_usage_snapshot'->>'observation_source', '') = 'active_probe'
    AND COALESCE(extra->'grok_usage_snapshot'->>'status_code', '') IN ('401', '402', '403')
    AND COALESCE(extra->'grok_usage_snapshot'->>'last_probe_at', '') <> ''
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\\t', QUOTE E'\b');
\"" > "${runtime_path}"

row_count="$(wc -l < "${runtime_path}" | tr -d ' ')"
echo "Pulled VPS failure observations: ${row_count}"
if [[ "${row_count}" == "0" ]]; then
  exit 0
fi

local_psql -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE vps_grok_oauth_runtime (
  id bigint PRIMARY KEY,
  credential_revision text NOT NULL,
  vps_status text,
  vps_schedulable boolean,
  status_code text NOT NULL,
  observation_source text NOT NULL,
  last_probe_at text NOT NULL,
  vps_updated_at timestamptz
);

\\copy vps_grok_oauth_runtime FROM '${runtime_path}' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH candidates AS (
  SELECT
    a.id,
    v.credential_revision,
    v.status_code,
    v.observation_source,
    v.last_probe_at,
    v.vps_status,
    v.vps_schedulable,
    v.vps_updated_at
  FROM public.accounts a
  JOIN vps_grok_oauth_runtime v ON v.id = a.id
  WHERE a.deleted_at IS NULL
    AND a.platform = 'grok'
    AND a.type = 'oauth'
    AND md5(a.credentials::text) = v.credential_revision
), updated AS (
  UPDATE public.accounts a
  SET extra = jsonb_set(
        COALESCE(a.extra, '{}'::jsonb),
        '{grok_vps_probe}',
        jsonb_build_object(
          'credential_revision', c.credential_revision,
          'status_code', c.status_code::integer,
          'observation_source', c.observation_source,
          'last_probe_at', c.last_probe_at,
          'vps_status', c.vps_status,
          'vps_schedulable', c.vps_schedulable,
          'vps_updated_at', c.vps_updated_at
        ),
        true
      )
  FROM candidates c
  WHERE a.id = c.id
    AND (
      COALESCE(a.extra->'grok_vps_probe'->>'credential_revision', '') <> c.credential_revision
      OR COALESCE(a.extra->'grok_vps_probe'->>'last_probe_at', '') < c.last_probe_at
    )
  RETURNING a.id
)
SELECT 'recorded_vps_grok_probe_observations=' || count(*) FROM updated;

SELECT 'ignored_vps_grok_probe_observations_credential_mismatch=' || count(*)
FROM vps_grok_oauth_runtime v
LEFT JOIN public.accounts a
  ON a.id = v.id
  AND a.deleted_at IS NULL
  AND a.platform = 'grok'
  AND a.type = 'oauth'
  AND md5(a.credentials::text) = v.credential_revision
WHERE a.id IS NULL;
SQL

echo "VPS Grok OAuth runtime pull completed."
