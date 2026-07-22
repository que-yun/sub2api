#!/usr/bin/env bash
set -euo pipefail

# One-way sync: local Anthropic setup-token credentials and configuration -> VPS.
# Does NOT delete VPS-only accounts. Local owns credentials/configuration; the VPS owns
# runtime state produced by traffic, including quota windows and passive usage samples.

REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
REMOTE_COPY_HOST="${REMOTE_COPY_HOST:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-15}"
SSH_RETRIES="${SSH_RETRIES:-4}"
SSH_RETRY_SLEEP_SECONDS="${SSH_RETRY_SLEEP_SECONDS:-8}"
LOCAL_PG_CONTAINER="${LOCAL_PG_CONTAINER:-sub2api-postgres}"
LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-127.0.0.1}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-5432}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${PGPASSWORD:-}}"
PGUSER="${PGUSER:-sub2api}"
PGDATABASE="${PGDATABASE:-sub2api}"
STANDBY_RESTART_AFTER_TOKEN_SYNC="${STANDBY_RESTART_AFTER_TOKEN_SYNC:-false}"
STANDBY_LOCAL_PROXY_ID="${STANDBY_LOCAL_PROXY_ID:-6}"
STANDBY_PROXY_POLICY="${STANDBY_PROXY_POLICY:-remap_local}"
STANDBY_REDIS_CONTAINER="${STANDBY_REDIS_CONTAINER:-sub2api-standby-redis}"
VERIFY_REMOTE_CREDENTIALS="${VERIFY_REMOTE_CREDENTIALS:-true}"
VERIFY_REMOTE_PROXY_HOST="${VERIFY_REMOTE_PROXY_HOST:-127.0.0.1}"
VERIFY_REMOTE_PROXY_PORT="${VERIFY_REMOTE_PROXY_PORT:-7891}"
VERIFY_REMOTE_MODEL="${VERIFY_REMOTE_MODEL:-claude-opus-4-8}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"
# If true, clear error/temp unschedulable and force active/schedulable when local is active.
RECOVER_REMOTE_STATUS="${RECOVER_REMOTE_STATUS:-true}"

tmp_dir="$(mktemp -d)"
dump_path="${tmp_dir}/anthropic-setup-tokens.tsv"
remote_suffix="$(date +%Y%m%d%H%M%S)-$$"
remote_path="/tmp/sub2api-anthropic-setup-tokens-${remote_suffix}.tsv"
remote_results_path="/tmp/sub2api-anthropic-setup-token-sync-results-${remote_suffix}.tsv"
lock_dir="${TMPDIR:-/tmp}/sub2api-anthropic-setup-token-sync.lock"

cleanup() {
  rm -rf "${tmp_dir}"
  rmdir "${lock_dir}" 2>/dev/null || true
}
trap cleanup EXIT

if ! mkdir "${lock_dir}" 2>/dev/null; then
  echo "Anthropic setup-token sync already running; skipping."
  exit 0
fi

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
    a.proxy_id,
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
remote_results_path_q="$(shell_quote "${remote_results_path}")"
service_q="$(shell_quote "${SERVICE_NAME}")"
restart_flag="${STANDBY_RESTART_AFTER_TOKEN_SYNC}"

remote_exec "set -euo pipefail
docker cp ${remote_path_q} sub2api-standby-postgres:${remote_path_q}
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
  proxy_id bigint,
  updated_at timestamptz NOT NULL,
  group_names text NOT NULL,
  credentials_changed boolean NOT NULL DEFAULT false,
  verification_pending boolean NOT NULL DEFAULT false
);

\\copy local_anthropic_setup_tokens (id, name, platform, type, credentials, extra, priority, status, schedulable, concurrency, proxy_id, updated_at, group_names) FROM '${remote_path}' WITH (FORMAT csv, DELIMITER E'\\t', QUOTE E'\\b');

UPDATE local_anthropic_setup_tokens l
SET credentials_changed = NOT EXISTS (
      SELECT 1
      FROM public.accounts a
      WHERE a.id = l.id
        AND a.deleted_at IS NULL
        AND a.platform = 'anthropic'
        AND a.type = 'setup-token'
        AND a.credentials = l.credentials
    ),
    verification_pending = EXISTS (
      SELECT 1
      FROM public.accounts a
      WHERE a.id = l.id
        AND a.deleted_at IS NULL
        AND a.platform = 'anthropic'
        AND a.type = 'setup-token'
        AND (
          a.error_message LIKE 'Pending VPS credential verification%'
          OR a.error_message LIKE 'Pending local credential sync%'
          OR a.error_message = 'VPS credential verification failed: HTTP 429'
          OR a.error_message LIKE 'VPS credential verification failed: HTTP 401%'
          OR a.error_message LIKE 'VPS credential verification failed: HTTP 403%'
        )
    );

WITH updated AS (
  UPDATE public.accounts a
  SET credentials = l.credentials,
      extra = COALESCE(a.extra, '{}'::jsonb) || (
        COALESCE(l.extra, '{}'::jsonb) - ARRAY[
          'model_rate_limits',
          'session_window_utilization',
          'passive_usage_7d_utilization',
          'passive_usage_7d_reset',
          'passive_usage_7d_oi_utilization',
          'passive_usage_7d_oi_reset',
          'passive_usage_sampled_at'
        ]
      ),
      proxy_id = CASE
        WHEN '${STANDBY_PROXY_POLICY}' = 'clear_all' THEN NULL
        WHEN l.proxy_id IS NULL THEN NULL
        ELSE ${STANDBY_LOCAL_PROXY_ID}::bigint
      END,
      priority = l.priority,
      concurrency = COALESCE(l.concurrency, a.concurrency),
      status = CASE
        WHEN '${recover_flag}' = 'true' AND l.credentials_changed AND l.status = 'active' THEN 'disabled'
        ELSE a.status
      END,
      schedulable = CASE
        WHEN '${recover_flag}' = 'true' AND l.credentials_changed AND l.schedulable THEN false
        ELSE a.schedulable
      END,
      error_message = CASE
        WHEN '${recover_flag}' = 'true' AND l.credentials_changed AND l.status = 'active' THEN 'Pending VPS credential verification'
        ELSE a.error_message
      END,
      temp_unschedulable_until = CASE
        WHEN '${recover_flag}' = 'true' AND l.credentials_changed AND l.status = 'active' THEN NULL
        ELSE a.temp_unschedulable_until
      END,
      temp_unschedulable_reason = CASE
        WHEN '${recover_flag}' = 'true' AND l.credentials_changed AND l.status = 'active' THEN NULL
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
    l.id, l.name, l.platform, l.type, l.credentials,
    COALESCE(l.extra, '{}'::jsonb) - ARRAY[
      'model_rate_limits',
      'session_window_utilization',
      'passive_usage_7d_utilization',
      'passive_usage_7d_reset',
      'passive_usage_7d_oi_utilization',
      'passive_usage_7d_oi_reset',
      'passive_usage_sampled_at'
    ],
    CASE
      WHEN '${STANDBY_PROXY_POLICY}' = 'clear_all' THEN NULL
      WHEN l.proxy_id IS NULL THEN NULL
      ELSE ${STANDBY_LOCAL_PROXY_ID}::bigint
    END,
    COALESCE(l.concurrency, 1), l.priority,
    CASE
      WHEN '${recover_flag}' = 'true' AND l.status = 'active' THEN 'disabled'
      ELSE l.status
    END,
    CASE
      WHEN '${recover_flag}' = 'true' AND l.status = 'active' AND l.schedulable THEN false
      ELSE l.schedulable
    END,
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

\\copy (SELECT id, credentials_changed, verification_pending, ('${recover_flag}' = 'true' AND status = 'active' AND schedulable) AS should_verify FROM local_anthropic_setup_tokens ORDER BY id) TO '${remote_results_path}' WITH (FORMAT csv, DELIMITER E'\\t', HEADER false);
SQL
docker exec sub2api-standby-postgres rm -f ${remote_path_q}
rm -f ${remote_path_q}
if [[ ${restart_flag} == true ]]; then
  systemctl restart ${service_q}
fi"

sync_results="$(remote_exec "docker exec sub2api-standby-postgres cat ${remote_results_path_q}")"
while IFS=$'\t' read -r account_id credentials_changed verification_pending should_verify; do
  [[ -n "${account_id}" ]] || continue
  if [[ "${credentials_changed}" != "t" && "${verification_pending}" != "t" ]]; then
    continue
  fi
  verify_after_sync="false"
  if [[ "${should_verify}" == "t" && ( "${credentials_changed}" == "t" || "${verification_pending}" == "t" ) ]]; then
    verify_after_sync="true"
  fi

  ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" bash -s -- \
    "${account_id}" \
    "${verify_after_sync}" \
    "${STANDBY_REDIS_CONTAINER}" \
    "${PGUSER}" \
    "${PGDATABASE}" \
    "${VERIFY_REMOTE_CREDENTIALS}" \
    "${VERIFY_REMOTE_PROXY_HOST}" \
    "${VERIFY_REMOTE_PROXY_PORT}" \
    "${VERIFY_REMOTE_MODEL}" <<'REMOTE'
set -euo pipefail

account_id="$1"
should_verify="$2"
redis_container="$3"
pguser="$4"
pgdatabase="$5"
verify_remote_credentials="$6"
proxy_host="$7"
proxy_port="$8"
verify_model="$9"

docker exec "${redis_container}" env -u REDISCLI_AUTH redis-cli DEL \
  "oauth:token:claude:account:${account_id}" \
  "oauth:refresh_lock:claude:account:${account_id}" >/dev/null
echo "Invalidated VPS Claude token cache: account_id=${account_id}"

if [[ "${should_verify}" != "true" || "${verify_remote_credentials}" != "true" ]]; then
  exit 0
fi

config_file="$(mktemp)"
body_file="$(mktemp)"
response_file="$(mktemp)"
cleanup_verify() {
  rm -f "${config_file}" "${body_file}" "${response_file}"
}
trap cleanup_verify EXIT

access_token="$(docker exec sub2api-standby-postgres psql -X -U "${pguser}" -d "${pgdatabase}" -Atc "SELECT credentials->>'access_token' FROM accounts WHERE id=${account_id} AND deleted_at IS NULL;")"
if [[ -z "${access_token}" ]]; then
  echo "Missing synced Anthropic access token: account_id=${account_id}" >&2
  exit 1
fi

chmod 600 "${config_file}" "${body_file}" "${response_file}"
printf 'header = "Authorization: Bearer %s"\n' "${access_token}" > "${config_file}"
cat > "${body_file}" <<JSON
{"model":"${verify_model}","messages":[{"role":"user","content":[{"type":"text","text":"Reply OK"}]}],"system":[{"type":"text","text":"You are Claude Code, Anthropic official CLI for Claude."}],"metadata":{"user_id":"user_vps_sync_verify_${account_id}"},"max_tokens":8,"temperature":1,"stream":false}
JSON
unset access_token

http_status="$(curl -sS --max-time 45 \
  --socks5-hostname "${proxy_host}:${proxy_port}" \
  --config "${config_file}" \
  -o "${response_file}" \
  -w '%{http_code}' \
  -X POST 'https://api.anthropic.com/v1/messages?beta=true' \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'anthropic-beta: claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14' \
  -H 'User-Agent: claude-cli/2.1.161 (external, cli)' \
  -H 'X-Stainless-Lang: js' \
  -H 'X-Stainless-Package-Version: 0.94.0' \
  -H 'X-Stainless-OS: Linux' \
  -H 'X-Stainless-Arch: arm64' \
  -H 'X-Stainless-Runtime: node' \
  -H 'X-Stainless-Runtime-Version: v24.3.0' \
  -H 'X-Stainless-Retry-Count: 0' \
  -H 'X-Stainless-Timeout: 600' \
  -H 'X-App: cli' \
  -H 'Anthropic-Dangerous-Direct-Browser-Access: true' \
  --data-binary "@${body_file}" || true)"

if [[ "${http_status}" == "200" || "${http_status}" == "429" ]]; then
  docker exec sub2api-standby-postgres psql -X -U "${pguser}" -d "${pgdatabase}" -v ON_ERROR_STOP=1 -c "
WITH verified AS (
  UPDATE public.accounts
  SET status = 'active',
      schedulable = true,
      error_message = NULL,
      temp_unschedulable_until = NULL,
      temp_unschedulable_reason = NULL,
      updated_at = now()
  WHERE id = ${account_id}
    AND deleted_at IS NULL
    AND platform = 'anthropic'
    AND type = 'setup-token'
  RETURNING id
)
INSERT INTO public.scheduler_outbox (event_type, account_id, group_id, payload)
SELECT 'account_changed', id, NULL, '{}'::jsonb FROM verified;
" >/dev/null
  echo "Verified VPS Anthropic credentials: account_id=${account_id} proxy=${proxy_host}:${proxy_port} HTTP=${http_status}"
  exit 0
fi

if [[ "${http_status}" != "401" && "${http_status}" != "403" ]]; then
  docker exec sub2api-standby-postgres psql -X -U "${pguser}" -d "${pgdatabase}" -v ON_ERROR_STOP=1 -c "
UPDATE public.accounts
SET status = 'disabled',
    schedulable = false,
    error_message = 'Pending VPS credential verification (last HTTP ${http_status})',
    updated_at = now()
WHERE id = ${account_id}
  AND deleted_at IS NULL
  AND platform = 'anthropic'
  AND type = 'setup-token';
" >/dev/null
  echo "VPS Anthropic credential verification pending: account_id=${account_id} proxy=${proxy_host}:${proxy_port} HTTP=${http_status}" >&2
  exit 1
fi

# 401/403 after local credentials were already written means local token itself is bad
# (or Anthropic temporarily rejects it). Keep schedulable=false, but use disabled rather
# than sticky error so the next local refresh+sync can re-verify without manual recover.
docker exec sub2api-standby-postgres psql -X -U "${pguser}" -d "${pgdatabase}" -v ON_ERROR_STOP=1 -c "
WITH failed AS (
  UPDATE public.accounts
  SET status = 'disabled',
      schedulable = false,
      error_message = 'VPS credential verification failed: HTTP ${http_status} (local credentials rejected; re-check on next local refresh)',
      temp_unschedulable_until = NULL,
      temp_unschedulable_reason = NULL,
      updated_at = now()
  WHERE id = ${account_id}
    AND deleted_at IS NULL
    AND platform = 'anthropic'
    AND type = 'setup-token'
  RETURNING id
)
INSERT INTO public.scheduler_outbox (event_type, account_id, group_id, payload)
SELECT 'account_changed', id, NULL, '{}'::jsonb FROM failed;
" >/dev/null
echo "VPS Anthropic credential verification failed: account_id=${account_id} proxy=${proxy_host}:${proxy_port} HTTP=${http_status}" >&2
exit 1
REMOTE
done <<< "${sync_results}"

remote_exec "docker exec sub2api-standby-postgres rm -f ${remote_results_path_q}"

echo "Anthropic setup-token sync completed."
