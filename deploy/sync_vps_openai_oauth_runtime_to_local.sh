#!/usr/bin/env bash
set -euo pipefail

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-15}"
SSH_RETRIES="${SSH_RETRIES:-4}"
SSH_RETRY_SLEEP_SECONDS="${SSH_RETRY_SLEEP_SECONDS:-8}"
REMOTE_QUERY_TIMEOUT_SECONDS="${REMOTE_QUERY_TIMEOUT_SECONDS:-45}"
LOCAL_PG_CONTAINER="${LOCAL_PG_CONTAINER:-sub2api-postgres}"
LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-127.0.0.1}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-5432}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${PGPASSWORD:-}}"
PGUSER="${PGUSER:-sub2api}"
PGDATABASE="${PGDATABASE:-sub2api}"
VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS="${VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS:-false}"

tmp_dir="$(mktemp -d)"
runtime_path="${tmp_dir}/vps-openai-oauth-runtime.tsv"

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


echo "Pulling VPS OpenAI OAuth runtime fields ..."
echo "VPS runtime permanent error import: ${VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS}"
remote_exec "timeout $(printf "%q" "${REMOTE_QUERY_TIMEOUT_SECONDS}") docker exec sub2api-standby-postgres psql -U $(printf "%q" "${PGUSER}") -d $(printf "%q" "${PGDATABASE}") -v ON_ERROR_STOP=1 -c \"
COPY (
  SELECT
    id,
    status,
    schedulable,
    error_message,
    rate_limited_at,
    rate_limit_reset_at,
    overload_until,
    session_window_status,
    temp_unschedulable_until,
    temp_unschedulable_reason,
    updated_at
  FROM public.accounts
  WHERE deleted_at IS NULL
    AND platform = 'openai'
    AND type = 'oauth'
    AND (
      status <> 'active'
      OR schedulable = false
      OR COALESCE(error_message, '') <> ''
      OR
      rate_limit_reset_at IS NOT NULL
      OR overload_until IS NOT NULL
      OR temp_unschedulable_until IS NOT NULL
      OR rate_limited_at IS NOT NULL
    )
) TO STDOUT WITH (FORMAT csv, DELIMITER E'\\t', QUOTE E'\\b');
\"" > "${runtime_path}"

row_count="$(wc -l < "${runtime_path}" | tr -d ' ')"
echo "Pulled runtime rows: ${row_count}"
if [[ "${row_count}" == "0" ]]; then
  exit 0
fi

local_psql -v ON_ERROR_STOP=1 <<SQL
CREATE TEMP TABLE vps_openai_oauth_runtime (
  id bigint PRIMARY KEY,
  status text,
  schedulable boolean,
  error_message text,
  rate_limited_at timestamptz,
  rate_limit_reset_at timestamptz,
  overload_until timestamptz,
  session_window_status text,
  temp_unschedulable_until timestamptz,
  temp_unschedulable_reason text,
  updated_at timestamptz
);

\\copy vps_openai_oauth_runtime FROM '${runtime_path}' WITH (FORMAT csv, DELIMITER E'\t', QUOTE E'\b');

WITH updated AS (
  UPDATE public.accounts a
  SET status = CASE
        WHEN '${VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS}' <> 'true' THEN a.status
        WHEN v.updated_at <= a.updated_at THEN a.status
        WHEN v.status IS NOT NULL AND v.status <> 'active' THEN v.status
        ELSE a.status
      END,
      schedulable = CASE
        WHEN '${VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS}' <> 'true' THEN a.schedulable
        WHEN v.updated_at <= a.updated_at THEN a.schedulable
        WHEN v.schedulable = false THEN false
        ELSE a.schedulable
      END,
      error_message = CASE
        WHEN '${VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS}' <> 'true' THEN a.error_message
        WHEN v.updated_at <= a.updated_at THEN a.error_message
        WHEN v.status IS NOT NULL AND v.status <> 'active' THEN COALESCE(NULLIF(v.error_message, ''), a.error_message)
        WHEN NULLIF(v.error_message, '') IS NOT NULL THEN v.error_message
        ELSE a.error_message
      END,
      rate_limited_at = CASE
        WHEN v.updated_at <= a.updated_at THEN a.rate_limited_at
        WHEN a.rate_limited_at IS NULL THEN v.rate_limited_at
        WHEN v.rate_limited_at IS NULL THEN a.rate_limited_at
        ELSE GREATEST(a.rate_limited_at, v.rate_limited_at)
      END,
      rate_limit_reset_at = CASE
        WHEN v.updated_at <= a.updated_at THEN a.rate_limit_reset_at
        WHEN a.rate_limit_reset_at IS NULL THEN v.rate_limit_reset_at
        WHEN v.rate_limit_reset_at IS NULL THEN a.rate_limit_reset_at
        ELSE GREATEST(a.rate_limit_reset_at, v.rate_limit_reset_at)
      END,
      overload_until = CASE
        WHEN v.updated_at <= a.updated_at THEN a.overload_until
        WHEN a.overload_until IS NULL THEN v.overload_until
        WHEN v.overload_until IS NULL THEN a.overload_until
        ELSE GREATEST(a.overload_until, v.overload_until)
      END,
      session_window_status = CASE
        WHEN v.updated_at <= a.updated_at THEN a.session_window_status
        ELSE COALESCE(v.session_window_status, a.session_window_status)
      END,
      temp_unschedulable_until = CASE
        WHEN v.updated_at <= a.updated_at THEN a.temp_unschedulable_until
        WHEN a.temp_unschedulable_until IS NULL THEN v.temp_unschedulable_until
        WHEN v.temp_unschedulable_until IS NULL THEN a.temp_unschedulable_until
        ELSE GREATEST(a.temp_unschedulable_until, v.temp_unschedulable_until)
      END,
      temp_unschedulable_reason = CASE
        WHEN v.updated_at <= a.updated_at THEN a.temp_unschedulable_reason
        WHEN v.temp_unschedulable_until IS NULL THEN a.temp_unschedulable_reason
        WHEN a.temp_unschedulable_until IS NULL THEN v.temp_unschedulable_reason
        WHEN v.temp_unschedulable_until >= a.temp_unschedulable_until THEN v.temp_unschedulable_reason
        ELSE a.temp_unschedulable_reason
      END,
      updated_at = CASE
        WHEN '${VPS_RUNTIME_PULL_IMPORT_PERMANENT_ERRORS}' = 'true' THEN GREATEST(a.updated_at, v.updated_at)
        ELSE a.updated_at
      END
  FROM vps_openai_oauth_runtime v
  WHERE a.id = v.id
    AND a.platform = 'openai'
    AND a.type = 'oauth'
  RETURNING a.id
)
SELECT 'merged_vps_openai_oauth_runtime_accounts=' || count(*) FROM updated;
SQL

echo "VPS OpenAI OAuth runtime pull completed."
