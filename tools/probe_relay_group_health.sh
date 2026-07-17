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
# Comma/space separated multi-group support. Example: "gpt-中转,通用"
GROUP_NAMES="${GROUP_NAMES:-}"
GROUP_MODEL_FAMILY="${GROUP_MODEL_FAMILY:-auto}"
# responses | chat | auto
# auto: gpt/codex groups -> responses; mixed/general groups -> chat; responses_unsupported -> chat fallback
PROBE_MODE="${PROBE_MODE:-auto}"
PROBE_MODEL="${PROBE_MODEL:-}"
PROBE_TIMEOUT_SECONDS="${PROBE_TIMEOUT_SECONDS:-75}"
PROBE_CONNECT_TIMEOUT_SECONDS="${PROBE_CONNECT_TIMEOUT_SECONDS:-10}"
MODELS_TIMEOUT_SECONDS="${MODELS_TIMEOUT_SECONDS:-20}"
PROBE_SLEEP_SECONDS="${PROBE_SLEEP_SECONDS:-15}"
PROBE_MAX_BODY_BYTES="${PROBE_MAX_BODY_BYTES:-1048576}"
# If 1, chronic unavailable/degraded accounts get soft priority demotion within tier bounds.
SOFT_PRIORITY_ADJUST="${SOFT_PRIORITY_ADJUST:-0}"
SOFT_PRIORITY_DEGRADED_BUMP="${SOFT_PRIORITY_DEGRADED_BUMP:-20}"
SOFT_PRIORITY_UNAVAILABLE_BUMP="${SOFT_PRIORITY_UNAVAILABLE_BUMP:-40}"
SOFT_PRIORITY_MAX="${SOFT_PRIORITY_MAX:-90}"
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

normalize_chat_url() {
  local base_url="$1"
  base_url="${base_url%%[[:space:]]*}"
  base_url="${base_url%/}"
  if [[ "$base_url" == */chat/completions ]]; then
    printf '%s\n' "$base_url"
  elif [[ "$base_url" == */v1 ]]; then
    printf '%s/chat/completions\n' "$base_url"
  else
    printf '%s/v1/chat/completions\n' "$base_url"
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
  elif [[ "$group_name_lc" == *"通用"* || "$group_name_lc" == *"mixed"* || "$group_name_lc" == *"general"* ]]; then
    printf '%s\n' "mixed"
  else
    # Non-gpt/claude named groups are treated as mixed OpenAI-compatible pools.
    printf '%s\n' "mixed"
  fi
}

resolve_probe_mode() {
  local family="$1"
  local mode_lc
  mode_lc="$(printf '%s' "$PROBE_MODE" | tr '[:upper:]' '[:lower:]')"
  case "$mode_lc" in
    responses|chat)
      printf '%s\n' "$mode_lc"
      ;;
    *)
      if [[ "$family" == "gpt" ]]; then
        printf '%s\n' "responses"
      else
        printf '%s\n' "chat"
      fi
      ;;
  esac
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
    mixed|*)
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
      def is_glob: test("[*]");
      . as $m
      | if $family == "mixed" then
          # Prefer concrete upstream targets (values), not request-side globs (keys).
          [($m | to_entries[] | .value // empty)]
          | map(select(type=="string" and length>0 and (is_glob|not) and allowed($family)))
          | .[0] // empty
        else
          ["gpt-5.5","gpt-5.4","gpt-5.3-codex","gpt-5.2","gpt-5.1-codex","gpt-5-codex","gpt-5"]
          | map(select(($m | has(.)) and (. | allowed($family)))) | .[0] // empty
        end
    ' <<< "${mapping_json:-{}}" 2>/dev/null || true
  )
  if [[ -n "$selected" ]]; then
    printf '%s\n' "$selected"
    return 0
  fi
  if [[ -n "$extra_model" ]] && [[ ! "$extra_model" =~ ^[0-9]+$ ]] && is_model_allowed_for_family "$extra_model" "$family"; then
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
      def is_glob: test("[*]");
      if $family == "mixed" then
        [to_entries[] | .value // empty]
        | map(select(type=="string" and length>0 and (is_glob|not) and allowed($family)))
        | .[0] // empty
      else
        keys_unsorted | map(select(allowed($family) and (is_glob|not))) | .[0] // empty
      end
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
    mixed)
      printf '%s\n' "gpt-4o-mini"
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

build_chat_payload() {
  local model="$1"
  "$JQ_BIN" -cn \
    --arg model "$model" \
    '{
      model: $model,
      messages: [
        {"role":"user","content":"Reply with exactly: ready"}
      ],
      max_tokens: 8,
      temperature: 0,
      stream: false
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
    if [[ "$body_lc" == *"model"* && ( "$body_lc" == *"not found"* || "$body_lc" == *"not_found"* || "$body_lc" == *"unsupported"* || "$body_lc" == *"不支持"* ) ]]; then
      printf '%s\t%s\n' "unavailable" "model_unsupported"
    elif [[ "$body_lc" == *"page not found"* || "$body_lc" == *"<html"* ]]; then
      printf '%s\t%s\n' "unavailable" "responses_unsupported"
    else
      printf '%s\t%s\n' "unavailable" "endpoint_unsupported"
    fi
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
  local probe_mode="${13:-}"
  local checked_at cooldown_minutes cooldown_reason soft_sql
  checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  cooldown_minutes="$(cooldown_minutes_for_category "$health_status" "$category")"
  cooldown_reason=""
  if [[ "${cooldown_minutes:-0}" -gt 0 ]]; then
    cooldown_reason="health_probe:${category}:cooldown_${cooldown_minutes}m"
  fi

  soft_sql="priority = priority"
  if [[ "${SOFT_PRIORITY_ADJUST}" == "1" ]]; then
    case "$health_status" in
      available)
        # Healthy probe: restore toward baseline if previously soft-bumped by this probe.
        soft_sql="priority = CASE
          WHEN COALESCE(extra->>'health_probe_soft_priority','') = '1'
            THEN GREATEST(COALESCE((extra->>'health_probe_priority_baseline')::int, priority), 1)
          ELSE priority
        END"
        ;;
      degraded)
        soft_sql="priority = LEAST(${SOFT_PRIORITY_MAX}, GREATEST(priority, COALESCE((extra->>'health_probe_priority_baseline')::int, priority) + ${SOFT_PRIORITY_DEGRADED_BUMP}))"
        ;;
      unavailable)
        soft_sql="priority = LEAST(${SOFT_PRIORITY_MAX}, GREATEST(priority, COALESCE((extra->>'health_probe_priority_baseline')::int, priority) + ${SOFT_PRIORITY_UNAVAILABLE_BUMP}))"
        ;;
    esac
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
      'health_probe_mode', '$(sql_escape "$probe_mode")',
      'health_probe_checked_at', '${checked_at}',
      'health_probe_priority_baseline', COALESCE(extra->'health_probe_priority_baseline', to_jsonb(priority)),
      'health_probe_soft_priority', CASE WHEN '${SOFT_PRIORITY_ADJUST}' = '1' THEN '1' ELSE COALESCE(extra->>'health_probe_soft_priority', '0') END
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
    ${soft_sql},
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
  local family mode model responses_url chat_url session_id payload body_file meta http_status time_total latency_ms curl_exit classification health_status category body error_short models_probe models_status models_json models_count request_url

  if [[ -z "$base_url" || -z "$api_key" ]]; then
    persist_result "$account_id" "unavailable" "missing_config" "" "" "" "$base_url" "missing base_url or api_key" "" "" "[]" "$group_id" "none"
    echo "account=${account_id} status=unavailable category=missing_config"
    return 0
  fi

  family="$(detect_group_model_family)"
  mode="$(resolve_probe_mode "$family")"

  # Claude family is not covered by OpenAI responses/chat probe here.
  if [[ "$family" == "claude" && "$mode" == "responses" ]]; then
    persist_result "$account_id" "unavailable" "unsupported_probe_family" "" "" "" "$base_url" "group family ${family} is not supported by current probe modes" "" "" "[]" "$group_id" "$mode"
    echo "account=${account_id} name=${name} status=unavailable category=unsupported_probe_family family=${family}"
    return 0
  fi

  models_probe="$(probe_models "$base_url" "$api_key")"
  models_status="${models_probe%%	*}"
  models_json="${models_probe#*	}"
  models_count="$("$JQ_BIN" 'length' <<< "${models_json:-[]}" 2>/dev/null || printf '0')"

  model="$(choose_model "$mapping_json" "$extra_model" "$family")"

  # For mixed/chat pools, prefer a real model id from /models when available.
  if [[ -z "${PROBE_MODEL}" && "$models_status" == "ok" && "${models_count:-0}" -gt 0 ]]; then
    preferred=""
    if [[ "$family" == "mixed" || "$(resolve_probe_mode "$family")" == "chat" ]]; then
      preferred="$("$JQ_BIN" -r '
        def rank:
          if test("(?i)embed|whisper|tts|image|vision|fuyu|diffusion|rerank|moderation|audio|transcri|sea-lion|70b|72b|405b|dbrx|jamba") then 90
          elif test("(?i)meta/llama-3\\.1-8b|gpt-oss:20b|flash|mini|small|lite|fast|8b|7b|9b") then 0
          elif test("(?i)llama-3\\.1-8b|meta/llama|gpt-oss|qwen|glm-5|deepseek|gemma|mistral|sensenova|minimax|kimi|nemotron") then 1
          elif test("(?i)gpt-|claude-|o1|o3|o4") then 2
          else 5 end;
        map(select(type=="string" and length>0 and (test("\\*")|not)))
        | sort_by(rank) | .[0] // empty
      ' <<< "${models_json:-[]}" 2>/dev/null || true)"
    fi
    if [[ -n "$preferred" ]]; then
      # Replace weak defaults / numeric garbage / empty.
      if [[ -z "$model" || "$model" =~ ^[0-9]+$ || "$model" == "gpt-4o-mini" || "$model" == "gpt-5.5" || "$model" == "claude-sonnet-4-5" ]]; then
        model="$preferred"
      else
        # Keep explicit mapping hit only if it exists in provider model list.
        if ! "$JQ_BIN" -e --arg m "$model" 'index($m) != null' <<< "${models_json:-[]}" >/dev/null 2>&1; then
          model="$preferred"
        fi
      fi
    fi
  fi

  # Provider-aware defaults for known mixed sources.
  base_lc="$(printf '%s' "$base_url" | tr '[:upper:]' '[:lower:]')"
  if [[ "$base_lc" == *"nvidia.com"* ]]; then
    if [[ -z "${PROBE_MODEL}" ]]; then
      model='meta/llama-3.1-8b-instruct'
    fi
  elif [[ "$base_lc" == *"ollama.com"* && -z "${PROBE_MODEL}" ]]; then
    if "$JQ_BIN" -e --arg m 'gpt-oss:20b' 'index($m) != null' <<< "${models_json:-[]}" >/dev/null 2>&1; then
      model='gpt-oss:20b'
    fi
  elif [[ "$base_lc" == *"sensenova"* && -z "${PROBE_MODEL}" ]]; then
    if "$JQ_BIN" -e --arg m 'sensenova-u1-fast' 'index($m) != null' <<< "${models_json:-[]}" >/dev/null 2>&1; then
      model='sensenova-u1-fast'
    elif "$JQ_BIN" -e --arg m 'glm-5.2' 'index($m) != null' <<< "${models_json:-[]}" >/dev/null 2>&1; then
      model='glm-5.2'
    fi
  fi

  if [[ -z "$model" || "$model" =~ ^[0-9]+$ ]]; then
    case "$family" in
      claude) model="claude-sonnet-4-5" ;;
      gpt) model="gpt-5.2" ;;
      *) model="gpt-4o-mini" ;;
    esac
  fi

  do_request() {
    local url="$1"
    local data_payload="$2"
    local accept_header="$3"
    body_file="$(mktemp -t sub2api-relay-probe.XXXXXX)"
    set +e
    meta=$(
      "$CURL_BIN" -sS --connect-timeout "$PROBE_CONNECT_TIMEOUT_SECONDS" --max-time "$PROBE_TIMEOUT_SECONDS" \
        -o "$body_file" \
        -w $'%{http_code}\t%{time_total}' \
        -X POST "$url" \
        -H "Authorization: Bearer ${api_key}" \
        -H "Content-Type: application/json" \
        -H "Accept: ${accept_header}" \
        -H "User-Agent: ${CODEX_USER_AGENT}" \
        --data-binary "$data_payload" 2>&1
    )
    curl_exit=$?
    set -e
    http_status="$(printf '%s' "$meta" | tail -n 1 | awk -F $'\t' '{print $1}')"
    time_total="$(printf '%s' "$meta" | tail -n 1 | awk -F $'\t' '{print $2}')"
    if [[ ! "$http_status" =~ ^[0-9][0-9][0-9]$ ]]; then
      http_status=""
      time_total=""
    fi
    latency_ms=""
    if [[ -n "$time_total" ]]; then
      latency_ms="$(awk -v t="$time_total" 'BEGIN { printf "%d", t * 1000 }')"
    fi
    body="$(head -c "$PROBE_MAX_BODY_BYTES" "$body_file" 2>/dev/null || true)"
    rm -f "$body_file"
  }

  if [[ "$mode" == "responses" ]]; then
    session_id="probe_relay_${account_id}_$(date +%s)"
    payload="$(build_payload "$model" "$session_id")"
    request_url="$(normalize_responses_url "$base_url")"
    # Keep codex-ish headers for GPT responses pools.
    body_file="$(mktemp -t sub2api-relay-probe.XXXXXX)"
    set +e
    meta=$(
      "$CURL_BIN" -sS --connect-timeout "$PROBE_CONNECT_TIMEOUT_SECONDS" --max-time "$PROBE_TIMEOUT_SECONDS" \
        -o "$body_file" \
        -w $'%{http_code}\t%{time_total}' \
        -X POST "$request_url" \
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
    if [[ ! "$http_status" =~ ^[0-9][0-9][0-9]$ ]]; then
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

    # Auto-fallback: responses unsupported -> chat for mixed-compatible endpoints.
    if [[ "$category" == "responses_unsupported" || "$category" == "invalid_codex_request" || "$category" == "model_unsupported" ]]; then
      mode="chat"
      payload="$(build_chat_payload "$model")"
      request_url="$(normalize_chat_url "$base_url")"
      do_request "$request_url" "$payload" "application/json"
      classification="$(classify_result "$http_status" "$curl_exit" "$body")"
      health_status="${classification%%	*}"
      category="${classification#*	}"
    fi
  else
    payload="$(build_chat_payload "$model")"
    request_url="$(normalize_chat_url "$base_url")"
    do_request "$request_url" "$payload" "application/json"
    classification="$(classify_result "$http_status" "$curl_exit" "$body")"
    health_status="${classification%%	*}"
    category="${classification#*	}"
  fi

  # One retry with next preferred chat model when first model clearly fails.
  if [[ "$mode" == "chat" && ( "$category" == "model_unsupported" || "$category" == "endpoint_unsupported" ) && "$models_status" == "ok" && "${models_count:-0}" -gt 1 ]]; then
    alt_model="$("$JQ_BIN" -r --arg cur "$model" '
      def rank:
        if test("(?i)embed|whisper|tts|image|vision|fuyu|diffusion|rerank|moderation|audio|transcri|sea-lion|70b|72b|405b|dbrx|jamba") then 90
        elif test("(?i)meta/llama-3\\.1-8b|gpt-oss:20b|flash|mini|small|lite|fast|8b|7b|9b|instruct") then 0
        elif test("(?i)llama|gpt-oss|qwen|glm|deepseek|gemma|mistral|sensenova|minimax|kimi|nemotron") then 1
        else 5 end;
      map(select(type=="string" and length>0 and . != $cur and (test("\\*")|not)))
      | sort_by(rank) | .[0] // empty
    ' <<< "${models_json:-[]}" 2>/dev/null || true)"
    if [[ -n "$alt_model" ]]; then
      model="$alt_model"
      payload="$(build_chat_payload "$model")"
      request_url="$(normalize_chat_url "$base_url")"
      do_request "$request_url" "$payload" "application/json"
      classification="$(classify_result "$http_status" "$curl_exit" "$body")"
      health_status="${classification%%	*}"
      category="${classification#*	}"
    fi
  fi

  if [[ "$curl_exit" != "0" ]]; then
    error_short="$(printf '%s' "$meta" | tr '\n' ' ' | cut -c 1-240)"
  else
    error_short="$(printf '%s' "$body" | tr '\n' ' ' | cut -c 1-240)"
  fi

  persist_result "$account_id" "$health_status" "$category" "$http_status" "$latency_ms" "$model" "$base_url" "$error_short" "$models_status" "$models_count" "$models_json" "$group_id" "$mode"
  echo "account=${account_id} name=${name} status=${health_status} category=${category} http=${http_status:-none} latency_ms=${latency_ms:-none} model=${model} mode=${mode} models=${models_status}:${models_count}"
}

probe_group() {
  local group_name="$1"
  local accounts account_id name base_url api_key mapping_json extra_model group_id
  GROUP_NAME="$group_name"
  GROUP_NAME_SQL="$(sql_escape "$GROUP_NAME")"

  log_info "relay group health probe start group=${GROUP_NAME} mode=${PROBE_MODE} family=$(detect_group_model_family) soft_priority=${SOFT_PRIORITY_ADJUST}"

  accounts=$(
    run_psql -At -P pager=off -c "
SELECT COALESCE(json_agg(row_to_json(t) ORDER BY id), '[]'::json)
FROM (
  SELECT
    a.id,
    a.name,
    COALESCE(a.credentials->>'base_url', '') AS base_url,
    COALESCE(a.credentials->>'api_key', '') AS api_key,
    COALESCE(a.credentials->'model_mapping', '{}'::jsonb) AS model_mapping,
    COALESCE(a.extra->>'default_model', '') AS default_model,
    g.id AS group_id
  FROM accounts a
  JOIN account_groups ag ON ag.account_id = a.id
  JOIN groups g ON g.id = ag.group_id
  WHERE a.deleted_at IS NULL
    AND g.deleted_at IS NULL
    AND g.name = '${GROUP_NAME_SQL}'
    AND a.platform = 'openai'
    AND a.type = 'apikey'
  ORDER BY a.id
) t;
"
  )

  if [[ -z "$accounts" || "$accounts" == "[]" ]]; then
    echo "no accounts to probe in group=${GROUP_NAME}"
    log_info "relay group health probe done group=${GROUP_NAME}"
    return 0
  fi

  while IFS= read -r row; do
    [[ -z "$row" ]] && continue
    account_id="$("$JQ_BIN" -r '.id // empty' <<< "$row")"
    name="$("$JQ_BIN" -r '.name // empty' <<< "$row")"
    base_url="$("$JQ_BIN" -r '.base_url // empty' <<< "$row")"
    api_key="$("$JQ_BIN" -r '.api_key // empty' <<< "$row")"
    mapping_json="$("$JQ_BIN" -c '.model_mapping // {}' <<< "$row")"
    extra_model="$("$JQ_BIN" -r '.default_model // empty' <<< "$row")"
    group_id="$("$JQ_BIN" -r '.group_id // empty' <<< "$row")"
    [[ -z "$account_id" ]] && continue
    probe_one "$account_id" "$name" "$base_url" "$api_key" "$mapping_json" "$extra_model" "$group_id" || true
    sleep "$PROBE_SLEEP_SECONDS"
  done < <("$JQ_BIN" -c '.[]' <<< "$accounts")

  log_info "relay group health probe done group=${GROUP_NAME}"
}

# Resolve target groups: GROUP_NAMES > GROUP_NAME
if [[ -n "${GROUP_NAMES// }" ]]; then
  # shellcheck disable=SC2206
  TARGET_GROUPS=(${GROUP_NAMES//,/ })
else
  TARGET_GROUPS=("$GROUP_NAME")
fi

for g in "${TARGET_GROUPS[@]}"; do
  [[ -z "${g// }" ]] && continue
  probe_group "$g" || true
done
