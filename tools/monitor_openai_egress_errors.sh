#!/bin/zsh
# 监控 sub2api ops_error_logs 中 OpenAI/Codex 出站传输故障（TLS/EOF/超时等）。
# 默认只告警、不自动切 Clash 节点，避免和 SUB2API-OAUTH-AUTO 抢控制权。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${REPO_ROOT}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"

export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"

if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

WINDOW_MINUTES="${WINDOW_MINUTES:-5}"
L1_THRESHOLD="${L1_THRESHOLD:-3}"
L2_THRESHOLD="${L2_THRESHOLD:-5}"
L2_FAILOVER_ATTEMPTS="${L2_FAILOVER_ATTEMPTS:-20}"
L2_FAILOVER_MIN_LOGS="${L2_FAILOVER_MIN_LOGS:-2}"
L3_THRESHOLD="${L3_THRESHOLD:-10}"
PROBE_ENABLED="${PROBE_ENABLED:-1}"
PROBE_TIMEOUT_SEC="${PROBE_TIMEOUT_SEC:-8}"
ALERT_COOLDOWN_SEC="${ALERT_COOLDOWN_SEC:-600}"
STATE_DIR="${STATE_DIR:-${ACTIVE_DEPLOY_DIR}/data-host/openai-egress-monitor}"
LOG_FILE="${LOG_FILE:-${HOME}/Library/Logs/sub2api-openai-egress-monitor.log}"
STATE_FILE="${STATE_DIR}/state.env"
LAST_ALERT_FILE="${STATE_DIR}/last_alert.env"

DB_HOST="${DATABASE_HOST:-127.0.0.1}"
DB_PORT="${DATABASE_PORT:-5433}"
DB_USER="${DATABASE_USER:-sub2api}"
DB_NAME="${DATABASE_DBNAME:-sub2api}"
DB_PASSWORD="${DATABASE_PASSWORD:-}"

SOCK5_OAUTH="${SOCK5_OAUTH:-127.0.0.1:17890}"
SOCK5_GENERAL="${SOCK5_GENERAL:-127.0.0.1:7891}"
MIHOMO_SOCK="${MIHOMO_SOCK:-}"

mkdir -p "${STATE_DIR}" "$(dirname "${LOG_FILE}")"

ts() { date '+%F %T'; }
log() {
  printf '%s %s\n' "$(ts)" "$*" | tee -a "${LOG_FILE}"
}

if [[ -z "${DB_PASSWORD}" ]]; then
  log "ERROR missing DATABASE_PASSWORD from ${ACTIVE_HOST_ENV}"
  exit 1
fi
if ! command -v psql >/dev/null 2>&1; then
  log "ERROR psql not found in PATH"
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  log "ERROR python3 not found in PATH"
  exit 1
fi

export PGPASSWORD="${DB_PASSWORD}"

SQL_STATS="$(cat <<SQL
WITH base AS (
  SELECT
    id,
    created_at,
    model,
    status_code,
    account_id,
    group_id,
    error_message,
    upstream_error_message,
    upstream_errors,
    CASE
      WHEN upstream_errors IS NULL THEN 0
      ELSE COALESCE(jsonb_array_length(upstream_errors), 0)
    END AS failover_attempts,
    CASE
      WHEN COALESCE(upstream_errors::text, '') ~* 'tls: first record does not look like a TLS handshake' THEN 'tls_handshake'
      WHEN COALESCE(upstream_errors::text, '') ~* 'tlsv1 alert protocol version|WRONG_VERSION_NUMBER|protocol version' THEN 'tls_version'
      WHEN COALESCE(upstream_errors::text, '') ~* 'i/o timeout|deadline exceeded|Client.Timeout' THEN 'timeout'
      WHEN COALESCE(upstream_errors::text, '') ~* 'connection reset' THEN 'conn_reset'
      WHEN COALESCE(upstream_errors::text, '') ~* 'connection refused' THEN 'conn_refused'
      WHEN COALESCE(upstream_errors::text, '') ~* 'EOF' THEN 'eof'
      WHEN COALESCE(upstream_error_message, '') ~* 'Upstream request failed' THEN 'upstream_request_failed'
      WHEN COALESCE(error_message, '') ~* 'Upstream service temporarily unavailable' THEN 'upstream_unavail'
      ELSE 'other_upstream'
    END AS kind
  FROM ops_error_logs
  WHERE created_at > NOW() - INTERVAL '${WINDOW_MINUTES} minutes'
    AND platform = 'openai'
    AND error_phase = 'upstream'
    AND status_code IN (502, 503)
    AND (
      model ILIKE 'gpt-5.5%'
      OR model ILIKE 'gpt-5.6%'
      OR group_id = 8
      OR COALESCE(upstream_errors::text, '') ILIKE '%chatgpt.com%'
      OR COALESCE(upstream_errors::text, '') ILIKE '%api.openai.com%'
      OR COALESCE(upstream_endpoint, '') ILIKE '%chatgpt.com%'
      OR COALESCE(upstream_endpoint, '') ILIKE '%openai.com%'
    )
    AND (
      COALESCE(upstream_errors::text, '') ~* 'tls: first record does not look like a TLS handshake'
      OR COALESCE(upstream_errors::text, '') ~* 'tlsv1 alert protocol version|WRONG_VERSION_NUMBER|protocol version'
      OR COALESCE(upstream_errors::text, '') ~* 'EOF|i/o timeout|deadline exceeded|Client.Timeout|connection reset|connection refused'
      OR COALESCE(upstream_error_message, '') ~* 'Upstream request failed'
      OR COALESCE(error_message, '') ~* 'Upstream service temporarily unavailable'
    )
)
SELECT json_build_object(
  'total', COUNT(*)::int,
  'deep_failover', COUNT(*) FILTER (WHERE failover_attempts >= ${L2_FAILOVER_ATTEMPTS})::int,
  'distinct_accounts', COUNT(DISTINCT account_id)::int,
  'distinct_models', COUNT(DISTINCT model)::int,
  'kinds', COALESCE(string_agg(DISTINCT kind, ',' ORDER BY kind), ''),
  'models', COALESCE(string_agg(DISTINCT model, ',' ORDER BY model), ''),
  'max_failover', COALESCE(MAX(failover_attempts), 0)::int,
  'first_hhmmss', COALESCE(MIN(to_char(created_at AT TIME ZONE 'Asia/Shanghai', 'HH24:MI:SS')), ''),
  'last_hhmmss', COALESCE(MAX(to_char(created_at AT TIME ZONE 'Asia/Shanghai', 'HH24:MI:SS')), '')
)::text
FROM base;
SQL
)"

stats_json="$(
  psql -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" -v ON_ERROR_STOP=1 -At -c "${SQL_STATS}" 2>&1
)" || {
  log "ERROR psql stats failed: ${stats_json}"
  exit 1
}

eval "$(
  STATS_JSON="${stats_json}" python3 - <<'PY'
import json, os, shlex
raw = os.environ["STATS_JSON"]
d = json.loads(raw)
def emit(k, v):
    print(f"{k}={shlex.quote(str(v))}")
emit("TOTAL", d.get("total", 0))
emit("DEEP_FAILOVER", d.get("deep_failover", 0))
emit("DISTINCT_ACCOUNTS", d.get("distinct_accounts", 0))
emit("DISTINCT_MODELS", d.get("distinct_models", 0))
emit("KINDS", d.get("kinds") or "-")
emit("MODELS", d.get("models") or "-")
emit("MAX_FAILOVER", d.get("max_failover", 0))
emit("FIRST_T", d.get("first_hhmmss") or "-")
emit("LAST_T", d.get("last_hhmmss") or "-")
PY
)" || {
  log "ERROR parse stats json failed: ${stats_json}"
  exit 1
}

TOTAL="${TOTAL:-0}"
DEEP_FAILOVER="${DEEP_FAILOVER:-0}"
DISTINCT_ACCOUNTS="${DISTINCT_ACCOUNTS:-0}"
MAX_FAILOVER="${MAX_FAILOVER:-0}"
KINDS="${KINDS:--}"
MODELS="${MODELS:--}"
FIRST_T="${FIRST_T:--}"
LAST_T="${LAST_T:--}"

PROXY_SUMMARY="-"
if (( TOTAL > 0 )); then
  SQL_PROXY="$(cat <<SQL
SELECT COALESCE(string_agg(fmt, ' | ' ORDER BY cnt DESC), '-')
FROM (
  SELECT p.port || '=' || COUNT(*)::text AS fmt, COUNT(*) AS cnt
  FROM ops_error_logs e
  JOIN accounts a ON a.id = e.account_id
  JOIN proxies p ON p.id = a.proxy_id
  WHERE e.created_at > NOW() - INTERVAL '${WINDOW_MINUTES} minutes'
    AND e.platform = 'openai'
    AND e.error_phase = 'upstream'
    AND e.status_code IN (502, 503)
    AND (
      e.model ILIKE 'gpt-5.5%'
      OR e.model ILIKE 'gpt-5.6%'
      OR e.group_id = 8
    )
  GROUP BY p.port
) s;
SQL
)"
  PROXY_SUMMARY="$(
    psql -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" -v ON_ERROR_STOP=1 -At -c "${SQL_PROXY}" 2>/dev/null || echo '-'
  )"
  PROXY_SUMMARY="${PROXY_SUMMARY:--}"
fi

resolve_mihomo_sock() {
  if [[ -n "${MIHOMO_SOCK}" && -S "${MIHOMO_SOCK}" ]]; then
    printf '%s' "${MIHOMO_SOCK}"
    return
  fi
  ls -t /tmp/mihomo-party-"$(id -u)"-*.sock 2>/dev/null | head -n 1 || true
}

CLASH_OAUTH_NOW="-"
CLASH_AUTO_NOW="-"
MIHOMO_SOCK_RESOLVED="$(resolve_mihomo_sock)"
if [[ -n "${MIHOMO_SOCK_RESOLVED}" && -S "${MIHOMO_SOCK_RESOLVED}" ]]; then
  CLASH_OAUTH_NOW="$(
    curl -sS --max-time 2 --unix-socket "${MIHOMO_SOCK_RESOLVED}" \
      "http://localhost/proxies/SUB2API-OAUTH" 2>/dev/null \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("now","-") or "-")' 2>/dev/null || echo '-'
  )"
  CLASH_AUTO_NOW="$(
    curl -sS --max-time 2 --unix-socket "${MIHOMO_SOCK_RESOLVED}" \
      "http://localhost/proxies/SUB2API-OAUTH-AUTO" 2>/dev/null \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("now","-") or "-")' 2>/dev/null || echo '-'
  )"
fi

probe_proxy() {
  local proxy="$1"
  local url="${2:-https://api.openai.com/v1/models}"
  local code
  code="$(
    curl -sS -o /dev/null -w '%{http_code}' \
      --max-time "${PROBE_TIMEOUT_SEC}" \
      --proxy "socks5h://${proxy}" \
      "${url}" 2>/dev/null || true
  )"
  if [[ "${code}" == "401" || "${code}" == "403" || "${code}" == "429" || "${code}" == "200" ]]; then
    printf 'ok:%s' "${code}"
  elif [[ "${code}" =~ ^[0-9]{3}$ ]]; then
    printf 'http:%s' "${code}"
  else
    printf 'fail:no_http'
  fi
}

PROBE_17890="skipped"
PROBE_7891="skipped"
if [[ "${PROBE_ENABLED}" == "1" ]]; then
  PROBE_17890="$(probe_proxy "${SOCK5_OAUTH}")"
  PROBE_7891="$(probe_proxy "${SOCK5_GENERAL}")"
fi

LEVEL="OK"
REASON="ok"
if (( TOTAL >= L3_THRESHOLD )); then
  LEVEL="L3"
  REASON="total>=${L3_THRESHOLD}"
elif (( TOTAL >= L2_THRESHOLD )); then
  LEVEL="L2"
  REASON="total>=${L2_THRESHOLD}"
elif (( DEEP_FAILOVER >= L2_FAILOVER_MIN_LOGS )); then
  LEVEL="L2"
  REASON="deep_failover_logs>=${L2_FAILOVER_MIN_LOGS}(attempts>=${L2_FAILOVER_ATTEMPTS})"
elif (( TOTAL >= L1_THRESHOLD )); then
  LEVEL="L1"
  REASON="total>=${L1_THRESHOLD}"
elif (( TOTAL > 0 )); then
  LEVEL="L0"
  REASON="noise"
fi

probe_bad() {
  local p="$1"
  [[ "${p}" == fail:* || "${p}" == http:000 || "${p}" == http:502 || "${p}" == http:503 ]]
}
if [[ "${LEVEL}" == "OK" || "${LEVEL}" == "L0" ]]; then
  if probe_bad "${PROBE_17890}" || probe_bad "${PROBE_7891}"; then
    LEVEL="L1"
    REASON="probe_degraded"
  fi
fi

ACTION="none"
case "${LEVEL}" in
  L1) ACTION="alert_only" ;;
  L2|L3) ACTION="alert_suggest_switch_SUB2API-OAUTH_AUTO" ;;
  *) ACTION="none" ;;
esac

SUMMARY="level=${LEVEL} total=${TOTAL} deep_failover=${DEEP_FAILOVER} accounts=${DISTINCT_ACCOUNTS} max_failover=${MAX_FAILOVER} kinds=${KINDS} models=${MODELS} proxies=${PROXY_SUMMARY} window=${WINDOW_MINUTES}m first=${FIRST_T} last=${LAST_T} probe17890=${PROBE_17890} probe7891=${PROBE_7891} oauth_now=${CLASH_OAUTH_NOW} auto_now=${CLASH_AUTO_NOW} reason=${REASON} action=${ACTION}"

{
  # 用 key=value 原样写入，避免 %q 弄乱 emoji/中文节点名
  printf 'LAST_RUN_TS=%s\n' "$(date +%s)"
  printf 'LEVEL=%s\n' "${LEVEL}"
  printf 'TOTAL=%s\n' "${TOTAL}"
  printf 'DEEP_FAILOVER=%s\n' "${DEEP_FAILOVER}"
  printf 'PROBE_17890=%s\n' "${PROBE_17890}"
  printf 'PROBE_7891=%s\n' "${PROBE_7891}"
  printf 'CLASH_OAUTH_NOW=%s\n' "${CLASH_OAUTH_NOW}"
  printf 'CLASH_AUTO_NOW=%s\n' "${CLASH_AUTO_NOW}"
  printf 'SUMMARY=%s\n' "${SUMMARY}"
} > "${STATE_FILE}"

should_alert=0
if [[ "${LEVEL}" == "L1" || "${LEVEL}" == "L2" || "${LEVEL}" == "L3" ]]; then
  should_alert=1
  if [[ -f "${LAST_ALERT_FILE}" ]]; then
    now_ts="$(date +%s)"
    last_ts="$(awk -F= '/^LAST_ALERT_TS=/{print $2; exit}' "${LAST_ALERT_FILE}" 2>/dev/null || true)"
    last_level="$(awk -F= '/^LAST_ALERT_LEVEL=/{print $2; exit}' "${LAST_ALERT_FILE}" 2>/dev/null || true)"
    if [[ "${last_ts}" =~ ^[0-9]+$ ]] && (( now_ts - last_ts < ALERT_COOLDOWN_SEC )) && [[ "${last_level}" == "${LEVEL}" ]]; then
      should_alert=0
    fi
  fi
fi

if (( should_alert )); then
  {
    printf 'LAST_ALERT_TS=%s\n' "$(date +%s)"
    printf 'LAST_ALERT_LEVEL=%s\n' "${LEVEL}"
    printf 'LAST_ALERT_SUMMARY=%s\n' "${SUMMARY}"
  } > "${LAST_ALERT_FILE}"

  log "ALERT ${SUMMARY}"
  log "HINT keep SUB2API-OAUTH=SUB2API-OAUTH-AUTO; if stuck, temporarily select next JP node then return to AUTO. Do NOT switch for grok/auth/account-pool errors."
  if command -v osascript >/dev/null 2>&1; then
    osascript -e "display notification \"${LEVEL}: ${TOTAL} openai egress errors / ${WINDOW_MINUTES}m\" with title \"sub2api OpenAI egress\"" >/dev/null 2>&1 || true
  fi
else
  if [[ "${LEVEL}" == "L1" || "${LEVEL}" == "L2" || "${LEVEL}" == "L3" ]]; then
    log "SUPPRESSED ${SUMMARY}"
  else
    log "OK ${SUMMARY}"
  fi
fi

case "${LEVEL}" in
  L1) exit 10 ;;
  L2) exit 20 ;;
  L3) exit 30 ;;
  *) exit 0 ;;
esac
