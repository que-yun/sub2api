#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${REPO_ROOT}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"

if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

LOCAL_PG_SOURCE="${LOCAL_PG_SOURCE:-host}"
LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5432}}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-${PGPASSWORD:-}}}"
DB_CONTAINER="${DB_CONTAINER:-sub2api-postgres}"
DB_USER="${DB_USER:-${DATABASE_USER:-sub2api}}"
DB_NAME="${DB_NAME:-${DATABASE_DBNAME:-sub2api}}"

GROUP_NAME="${GROUP_NAME:-gpt-中转}"
GROUP_MODEL_FAMILY="${GROUP_MODEL_FAMILY:-auto}"
PROBE_MODEL="${PROBE_MODEL:-}"
PROBE_TIMEOUT_SECONDS="${PROBE_TIMEOUT_SECONDS:-75}"
PROBE_CONNECT_TIMEOUT_SECONDS="${PROBE_CONNECT_TIMEOUT_SECONDS:-10}"
MODELS_TIMEOUT_SECONDS="${MODELS_TIMEOUT_SECONDS:-20}"
PROBE_SLEEP_SECONDS="${PROBE_SLEEP_SECONDS:-15}"
PROBE_MAX_BODY_BYTES="${PROBE_MAX_BODY_BYTES:-1048576}"
CODEX_VERSION="${CODEX_VERSION:-0.31.0}"
CODEX_USER_AGENT="${CODEX_USER_AGENT:-codex_cli_rs/${CODEX_VERSION}}"

CURL_BIN="${CURL_BIN:-$(command -v curl)}"
JQ_BIN="${JQ_BIN:-$(command -v jq)}"
PSQL_BIN="${PSQL_BIN:-$(command -v psql)}"

log_info() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S %z')] $*"
}

run_psql() {
  case "${LOCAL_PG_SOURCE}" in
    host)
      if [[ -z "${LOCAL_PG_PASSWORD}" ]]; then
        echo "LOCAL_PG_PASSWORD or PGPASSWORD is required when LOCAL_PG_SOURCE=host" >&2
        exit 1
      fi
      PGPASSWORD="${LOCAL_PG_PASSWORD}" "$PSQL_BIN" -h "${LOCAL_PG_HOST}" -p "${LOCAL_PG_PORT}" -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 "$@"
      ;;
    docker)
      docker exec -i "$DB_CONTAINER" psql -v ON_ERROR_STOP=1 -U "$DB_USER" -d "$DB_NAME" "$@"
      ;;
    *)
      echo "Unsupported LOCAL_PG_SOURCE=${LOCAL_PG_SOURCE}. Use host or docker." >&2
      exit 1
      ;;
  esac
}

sql_escape() {
  printf "%s" "$1" | sed "s/'/''/g"
}

json_string_sql() {
  "$JQ_BIN" -Rn --arg v "$1" '$v' | sed "s/'/''/g"
}

json_sql() {
  local value="$1"
  if [[ -z "$value" ]] || ! "$JQ_BIN" -e . >/dev/null 2>&1 <<< "$value"; then
    value="[]"
  fi
  printf "%s" "$value" | sed "s/'/''/g"
}

normalize_responses_url() {
  local base_url="$1"
  base_url="${base_url%%[[:space:]]*}"
  base_url="${base_url%/}"
  if [[ "$base_url" == */responses ]]; then
    printf '%s\n' "$base_url"
  elif [[ "$base_url" == */v1 ]]; then
    printf '%s/responses\n' "$base_url"
  else
    printf '%s/v1/responses\n' "$base_url"
  fi
}

normalize_models_url() {
  local base_url="$1"
  base_url="${base_url%%[[:space:]]*}"
  base_url="${base_url%/}"
  if [[ "$base_url" == */models ]]; then
    printf '%s\n' "$base_url"
  elif [[ "$base_url" == */v1 ]]; then
    printf '%s/models\n' "$base_url"
  else
    printf '%s/v1/models\n' "$base_url"
  fi
}

detect_group_model_family() {
  local group_name_lc
  group_name_lc="$(printf '%s' "$GROUP_NAME" | tr '[:upper:]' '[:lower:]')"
  if [[ "$GROUP_MODEL_FAMILY" != "auto" && -n "$GROUP_MODEL_FAMILY" ]]; then
    printf '%s\n' "$GROUP_MODEL_FAMILY"
  elif [[ "$group_name_lc" == *"gpt"* || "$group_name_lc" == *"codex"* ]]; then
    printf '%s\n' "gpt"
  elif [[ "$group_name_lc" == *"claude"* || "$group_name_lc" == *"anthropic"* ]]; then
    printf '%s\n' "claude"
  else
    printf '%s\n' "gpt"
  fi
}

is_model_allowed_for_family() {
  local model="$1"
  local family="$2"
  local model_lc
  model_lc="$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]')"

  case "$family" in
    gpt)
      [[ "$model_lc" == gpt-* || "$model_lc" == *codex* ]]
      ;;
    claude)
      [[ "$model_lc" == claude-* ]]
      ;;
    *)
      return 0
      ;;
  esac
}

choose_model() {
  local mapping_json="$1"
  local extra_model="$2"
  local family="$3"
  local selected

  if [[ -n "${PROBE_MODEL}" ]]; then
    if is_model_allowed_for_family "$PROBE_MODEL" "$family"; then
      printf '%s\n' "${PROBE_MODEL}"
      return 0
    fi
    log_info "ignore PROBE_MODEL=${PROBE_MODEL}: not allowed for family=${family}"
  fi

  selected=$(
    "$JQ_BIN" -r --arg family "$family" '
      def allowed($family):
        if $family == "gpt" then
          test("^(gpt-|.*codex.*)")
        elif $family == "claude" then
          test("^claude-")
        else
          true
        end;
      . as $m
      | ["gpt-5.5","gpt-5.4","gpt-5.3-codex","gpt-5.2","gpt-5.1-codex","gpt-5-codex","gpt-5"]
      | map(select(($m | has(.)) and (. | allowed($family)))) | .[0] // empty
    ' <<< "${mapping_json:-{}}" 2>/dev/null || true
  )
  if [[ -n "$selected" ]]; then
    printf '%s\n' "$selected"
    return 0
  fi
  if [[ -n "$extra_model" ]] && is_model_allowed_for_family "$extra_model" "$family"; then
    printf '%s\n' "$extra_model"
    return 0
  fi
  selected=$(
    "$JQ_BIN" -r --arg family "$family" '
      def allowed($family):
        if $family == "gpt" then
          test("^(gpt-|.*codex.*)")
        elif $family == "claude" then
          test("^claude-")
        else
          true
        end;
      keys_unsorted | map(select(allowed($family))) | .[0] // empty
    ' <<< "${mapping_json:-{}}" 2>/dev/null || true
  )
  if [[ -n "$selected" ]]; then
    printf '%s\n' "$selected"
    return 0
  fi
  case "$family" in
    claude)
      printf '%s\n' "claude-sonnet-4-5"
      ;;
    *)
      printf '%s\n' "gpt-5.5"
      ;;
  esac
}

build_payload() {
  local model="$1"
  local session_id="$2"
  "$JQ_BIN" -cn \
    --arg model "$model" \
    --arg session_id "$session_id" \
    '{
      model: $model,
      input: [{
        role: "user",
        content: [{
          type: "input_text",
          text: "Reply with: ready"
        }]
      }],
      instructions: "You are Codex, a coding agent based on GPT-5.",
      stream: true,
      store: false,
      reasoning: {
        effort: "medium",
        summary: "auto"
      },
      include: ["reasoning.encrypted_content"],
      prompt_cache_key: $session_id
    }'
}

classify_result() {
  local http_status_arg="$1"
  local curl_exit="$2"
  local body="$3"
  local body_lc
  body_lc="$(printf '%s' "$body" | tr '[:upper:]' '[:lower:]')"

  if [[ "$curl_exit" != "0" ]]; then
    printf '%s\t%s\n' "degraded" "request_error"
  elif [[ "$http_status_arg" =~ '^2' ]]; then
    printf '%s\t%s\n' "available" "ok"
  elif [[ "$http_status_arg" == "401" && ( "$body_lc" == *"invalid_api_key"* || "$body_lc" == *"incorrect api key"* || "$body_lc" == *"unauthorized"* || "$body_lc" == *"authentication"* ) ]]; then
    printf '%s\t%s\n' "unavailable" "auth_invalid"
  elif [[ "$http_status_arg" == "401" ]]; then
    printf '%s\t%s\n' "degraded" "auth_challenge"
  elif [[ "$http_status_arg" == "403" && ( "$body_lc" == *"<html"* || "$body_lc" == *"cloudflare"* || "$body_lc" == *"access denied"* || "$body_lc" == *"forbidden"* ) ]]; then
    printf '%s\t%s\n' "degraded" "site_blocked"
  elif [[ "$http_status_arg" == "403" && ( "$body_lc" == *"invalid_api_key"* || "$body_lc" == *"incorrect api key"* || "$body_lc" == *"unauthorized"* || "$body_lc" == *"authentication"* ) ]]; then
    printf '%s\t%s\n' "unavailable" "auth_invalid"
  elif [[ "$http_status_arg" == "403" ]]; then
    printf '%s\t%s\n' "degraded" "site_blocked"
  elif [[ "$http_status_arg" == "402" || "$body_lc" == *"insufficient"* || "$body_lc" == *"quota"* || "$body_lc" == *"balance"* || "$body_lc" == *"credit"* ]]; then
    printf '%s\t%s\n' "unavailable" "quota_exhausted"
  elif [[ "$http_status_arg" == "429" ]]; then
    printf '%s\t%s\n' "degraded" "rate_limited"
  elif [[ "$http_status_arg" == "404" || "$http_status_arg" == "405" ]]; then
    printf '%s\t%s\n' "unavailable" "responses_unsupported"
  elif [[ "$http_status_arg" == "413" ]]; then
    printf '%s\t%s\n' "unavailable" "payload_too_large"
  elif [[ "$http_status_arg" == "400" && ( "$body_lc" == *"model"* || "$body_lc" == *"does not exist"* || "$body_lc" == *"not support"* || "$body_lc" == *"unsupported"* ) ]]; then
    printf '%s\t%s\n' "unavailable" "model_unsupported"
  elif [[ "$http_status_arg" == "400" && "$body_lc" == *"invalid"* ]]; then
    printf '%s\t%s\n' "unavailable" "invalid_codex_request"
  elif [[ "$http_status_arg" =~ '^5' ]]; then
    printf '%s\t%s\n' "degraded" "temporary_5xx"
  else
    printf '%s\t%s\n' "degraded" "unexpected_status"
  fi
}

cooldown_minutes_for_category() {
  local health_status="$1"
  local category="$2"

  case "$category" in
    site_blocked)
      printf '%s\n' "30"
      ;;
    request_error|temporary_5xx)
      printf '%s\n' "10"
      ;;
    auth_challenge|rate_limited|unexpected_status)
      printf '%s\n' "15"
      ;;
    invalid_codex_request|model_unsupported|responses_unsupported|payload_too_large)
      printf '%s\n' "120"
      ;;
    *)
      if [[ "$health_status" == "degraded" ]]; then
        printf '%s\n' "10"
      else
        printf '%s\n' "0"
      fi
      ;;
  esac
}

persist_result() {
  local account_id="$1"
  local health_status="$2"
  local category="$3"
  local http_status="$4"
  local latency_ms="$5"
  local model="$6"
  local base_url="$7"
  local error_short="$8"
  local models_status="${9:-}"
  local models_count="${10:-}"
  local models_json="${11:-[]}"
  local group_id="${12:-}"
  local checked_at cooldown_minutes cooldown_reason
  checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  cooldown_minutes="$(cooldown_minutes_for_category "$health_status" "$category")"
  cooldown_reason=""
  if [[ "${cooldown_minutes:-0}" -gt 0 ]]; then
    cooldown_reason="health_probe:${category}:cooldown_${cooldown_minutes}m"
  fi

  run_psql -q -c "
UPDATE accounts
SET extra = COALESCE(extra, '{}'::jsonb) || jsonb_build_object(
      'health_probe_status', '$(sql_escape "$health_status")',
      'health_probe_category', '$(sql_escape "$category")',
      'health_probe_http_status', NULLIF('$(sql_escape "$http_status")', '')::int,
      'health_probe_latency_ms', NULLIF('$(sql_escape "$latency_ms")', '')::bigint,
      'health_probe_model', '$(json_string_sql "$model")'::jsonb,
      'health_probe_base_url', '$(json_string_sql "$base_url")'::jsonb,
      'health_probe_error', '$(json_string_sql "$error_short")'::jsonb,
      'health_probe_models_status', '$(sql_escape "$models_status")',
      'health_probe_models_count', COALESCE(NULLIF('$(sql_escape "$models_count")', '')::int, 0),
      'health_probe_models', '$(json_sql "$models_json")'::jsonb,
      'health_probe_checked_at', '${checked_at}'
    ),
    temp_unschedulable_until = CASE
      WHEN ${cooldown_minutes} > 0 THEN GREATEST(COALESCE(temp_unschedulable_until, NOW()), NOW() + (${cooldown_minutes} || ' minutes')::interval)
      WHEN temp_unschedulable_reason LIKE 'health_probe:%' THEN NULL
      ELSE temp_unschedulable_until
    END,
    temp_unschedulable_reason = CASE
      WHEN ${cooldown_minutes} > 0 THEN '$(sql_escape "$cooldown_reason")'
      WHEN temp_unschedulable_reason LIKE 'health_probe:%' THEN NULL
      ELSE temp_unschedulable_reason
    END,
    updated_at = NOW()
WHERE id = ${account_id}
  AND deleted_at IS NULL;
"

  run_psql -q -c "
INSERT INTO account_health_probe_results (
  account_id, group_id, group_name, status, category, http_status, latency_ms,
  model, base_url, models_count, models_status, error_message, checked_at
)
VALUES (
  ${account_id},
  NULLIF('$(sql_escape "$group_id")', '')::bigint,
  '$(sql_escape "$GROUP_NAME")',
  '$(sql_escape "$health_status")',
  '$(sql_escape "$category")',
  NULLIF('$(sql_escape "$http_status")', '')::int,
  NULLIF('$(sql_escape "$latency_ms")', '')::bigint,
  '$(sql_escape "$model")',
  '$(sql_escape "$base_url")',
  COALESCE(NULLIF('$(sql_escape "$models_count")', '')::int, 0),
  '$(sql_escape "$models_status")',
  '$(sql_escape "$error_short")',
  '${checked_at}'
);
" || true
}

probe_models() {
  local base_url="$1"
  local api_key="$2"
  local models_url model_body_file meta curl_exit http_status models_json models_count models_status model_body_lc
  models_url="$(normalize_models_url "$base_url")"
  model_body_file="$(mktemp -t sub2api-relay-models.XXXXXX)"

  set +e
  meta=$(
    "$CURL_BIN" -sS --connect-timeout "$PROBE_CONNECT_TIMEOUT_SECONDS" --max-time "$MODELS_TIMEOUT_SECONDS" \
      -o "$model_body_file" \
      -w '%{http_code}' \
      -X GET "$models_url" \
      -H "Authorization: Bearer ${api_key}" \
      -H "Accept: application/json" 2>&1
  )
  curl_exit=$?
  set -e

  http_status="$(printf '%s' "$meta" | tail -n 1)"
  if [[ ! "$http_status" =~ '^[0-9][0-9][0-9]$' ]]; then
    http_status=""
  fi

  if [[ "$curl_exit" != "0" ]]; then
    models_status="request_error"
    models_json="[]"
  elif [[ "$http_status" =~ '^2' ]]; then
    models_json=$(
      "$JQ_BIN" -c '
        if (.data | type) == "array" then
          [.data[] | (.id // .name // empty)] | map(select(. != ""))
        elif (.models | type) == "array" then
          [.models[] | if type == "string" then . else (.id // .name // empty) end] | map(select(. != ""))
        else
          []
        end
      ' "$model_body_file" 2>/dev/null || printf '[]'
    )
    models_count="$("$JQ_BIN" 'length' <<< "$models_json" 2>/dev/null || printf '0')"
    if [[ "${models_count:-0}" -gt 0 ]]; then
      models_status="ok"
    else
      models_status="empty"
    fi
  elif [[ "$http_status" == "404" || "$http_status" == "405" ]]; then
    models_status="unsupported"
    models_json="[]"
  elif [[ "$http_status" == "401" || "$http_status" == "403" ]]; then
    model_body_lc="$(tr '[:upper:]' '[:lower:]' < "$model_body_file" 2>/dev/null | head -c "$PROBE_MAX_BODY_BYTES" || true)"
    if [[ "$model_body_lc" == *"invalid_api_key"* || "$model_body_lc" == *"incorrect api key"* || "$model_body_lc" == *"unauthorized"* || "$model_body_lc" == *"authentication"* ]]; then
      models_status="auth_invalid"
    elif [[ "$model_body_lc" == *"<html"* || "$model_body_lc" == *"cloudflare"* || "$model_body_lc" == *"access denied"* || "$model_body_lc" == *"forbidden"* ]]; then
      models_status="site_blocked"
    else
      models_status="http_${http_status}"
    fi
    models_json="[]"
  else
    models_status="http_${http_status}"
    models_json="[]"
  fi
  rm -f "$model_body_file"

  printf '%s\t%s\n' "$models_status" "${models_json:-[]}"
}

probe_one() {
  local account_id="$1"
  local name="$2"
  local base_url="$3"
  local api_key="$4"
  local mapping_json="$5"
  local extra_model="$6"
  local group_id="$7"
  local family model responses_url session_id payload body_file meta http_status time_total latency_ms curl_exit classification health_status category body error_short models_probe models_status models_json models_count

  if [[ -z "$base_url" || -z "$api_key" ]]; then
    persist_result "$account_id" "unavailable" "missing_config" "" "" "" "$base_url" "missing base_url or api_key" "" "" "[]" "$group_id"
    echo "account=${account_id} status=unavailable category=missing_config"
    return 0
  fi

  family="$(detect_group_model_family)"
  if [[ "$family" != "gpt" ]]; then
    persist_result "$account_id" "unavailable" "unsupported_probe_family" "" "" "" "$base_url" "group family ${family} is not supported by OpenAI Responses probe" "" "" "[]" "$group_id"
    echo "account=${account_id} name=${name} status=unavailable category=unsupported_probe_family family=${family}"
    return 0
  fi

  model="$(choose_model "$mapping_json" "$extra_model" "$family")"
  models_probe="$(probe_models "$base_url" "$api_key")"
  models_status="${models_probe%%	*}"
  models_json="${models_probe#*	}"
  models_count="$("$JQ_BIN" 'length' <<< "${models_json:-[]}" 2>/dev/null || printf '0')"
  responses_url="$(normalize_responses_url "$base_url")"
  session_id="probe_relay_${account_id}_$(date +%s)"
  payload="$(build_payload "$model" "$session_id")"
  body_file="$(mktemp -t sub2api-relay-probe.XXXXXX)"

  set +e
  meta=$(
    "$CURL_BIN" -sS --connect-timeout "$PROBE_CONNECT_TIMEOUT_SECONDS" --max-time "$PROBE_TIMEOUT_SECONDS" \
      -o "$body_file" \
      -w $'%{http_code}\t%{time_total}' \
      -X POST "$responses_url" \
      -H "Authorization: Bearer ${api_key}" \
      -H "Content-Type: application/json" \
      -H "Accept: text/event-stream" \
      -H "OpenAI-Beta: responses=experimental" \
      -H "Originator: codex_cli_rs" \
      -H "Version: ${CODEX_VERSION}" \
      -H "User-Agent: ${CODEX_USER_AGENT}" \
      -H "Session_ID: ${session_id}" \
      -H "Conversation_ID: ${session_id}" \
      --data-binary "$payload" 2>&1
  )
  curl_exit=$?
  set -e

  http_status="$(printf '%s' "$meta" | tail -n 1 | awk -F $'\t' '{print $1}')"
  time_total="$(printf '%s' "$meta" | tail -n 1 | awk -F $'\t' '{print $2}')"
  if [[ ! "$http_status" =~ '^[0-9][0-9][0-9]$' ]]; then
    http_status=""
    time_total=""
  fi
  latency_ms=""
  if [[ -n "$time_total" ]]; then
    latency_ms="$(awk -v t="$time_total" 'BEGIN { printf "%d", t * 1000 }')"
  fi

  body="$(head -c "$PROBE_MAX_BODY_BYTES" "$body_file" 2>/dev/null || true)"
  rm -f "$body_file"

  classification="$(classify_result "$http_status" "$curl_exit" "$body")"
  health_status="${classification%%	*}"
  category="${classification#*	}"
  if [[ "$curl_exit" != "0" ]]; then
    error_short="$(printf '%s' "$meta" | tr '\n' ' ' | cut -c 1-240)"
  else
    error_short="$(printf '%s' "$body" | tr '\n' ' ' | cut -c 1-240)"
  fi

  persist_result "$account_id" "$health_status" "$category" "$http_status" "$latency_ms" "$model" "$base_url" "$error_short" "$models_status" "$models_count" "$models_json" "$group_id"
  echo "account=${account_id} name=${name} status=${health_status} category=${category} http=${http_status:-none} latency_ms=${latency_ms:-none} model=${model} models=${models_status}:${models_count}"
}

GROUP_NAME_SQL="$(sql_escape "$GROUP_NAME")"

log_info "relay group health probe start group=${GROUP_NAME}"

ACCOUNTS=$(
  run_psql -At -F $'\t' -P pager=off -c "
SELECT
  a.id,
  replace(a.name, E'\t', ' '),
  COALESCE(a.credentials->>'base_url', ''),
  COALESCE(a.credentials->>'api_key', ''),
  COALESCE(a.credentials->'model_mapping', '{}'::jsonb)::text,
  COALESCE(a.extra->>'default_model', ''),
  g.id
FROM accounts a
JOIN account_groups ag ON ag.account_id = a.id
JOIN groups g ON g.id = ag.group_id
WHERE a.deleted_at IS NULL
  AND g.deleted_at IS NULL
  AND g.name = '${GROUP_NAME_SQL}'
  AND a.platform = 'openai'
  AND a.type = 'apikey'
ORDER BY a.id;
"
)

if [[ -z "$ACCOUNTS" ]]; then
  echo "no accounts to probe"
  log_info "relay group health probe done"
  exit 0
fi

while IFS=$'\t' read -r account_id name base_url api_key mapping_json extra_model group_id; do
  [[ -z "$account_id" ]] && continue
  probe_one "$account_id" "$name" "$base_url" "$api_key" "$mapping_json" "$extra_model" "$group_id" || true
  sleep "$PROBE_SLEEP_SECONDS"
done <<< "$ACCOUNTS"

log_info "relay group health probe done"
