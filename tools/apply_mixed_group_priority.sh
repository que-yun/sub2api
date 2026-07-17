#!/bin/zsh
set -euo pipefail

# Apply A/B/C/D priority tiers for mixed OpenAI apikey groups.
# Default target: 通用
#
# Usage:
#   ./tools/apply_mixed_group_priority.sh              # dry-run
#   ./tools/apply_mixed_group_priority.sh --apply      # write
#   GROUP_NAME=通用 ./tools/apply_mixed_group_priority.sh --apply

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ACTIVE_DEPLOY_DIR="${ACTIVE_DEPLOY_DIR:-${REPO_ROOT}/deploy}"
ACTIVE_HOST_ENV="${ACTIVE_HOST_ENV:-${ACTIVE_DEPLOY_DIR}/host-run.env}"

if [[ -f "${ACTIVE_HOST_ENV}" ]]; then
  set -a
  source "${ACTIVE_HOST_ENV}"
  set +a
fi

GROUP_NAME="${GROUP_NAME:-通用}"
APPLY=0
if [[ "${1:-}" == "--apply" ]]; then
  APPLY=1
fi

LOCAL_PG_HOST="${LOCAL_PG_HOST:-${DATABASE_HOST:-127.0.0.1}}"
LOCAL_PG_PORT="${LOCAL_PG_PORT:-${DATABASE_PORT:-5432}}"
LOCAL_PG_PASSWORD="${LOCAL_PG_PASSWORD:-${DATABASE_PASSWORD:-${PGPASSWORD:-}}}"
DB_USER="${DB_USER:-${DATABASE_USER:-sub2api}}"
DB_NAME="${DB_NAME:-${DATABASE_DBNAME:-sub2api}}"
PSQL_BIN="${PSQL_BIN:-$(command -v psql)}"

if [[ -z "${LOCAL_PG_PASSWORD}" ]]; then
  echo "DATABASE_PASSWORD/PGPASSWORD required" >&2
  exit 1
fi

run_psql() {
  PGPASSWORD="${LOCAL_PG_PASSWORD}" "$PSQL_BIN" -h "${LOCAL_PG_HOST}" -p "${LOCAL_PG_PORT}" -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 "$@"
}

sql_escape() {
  printf "%s" "$1" | sed "s/'/''/g"
}

GROUP_NAME_SQL="$(sql_escape "$GROUP_NAME")"

# Tier map for 通用. Edit here when members change.
# A=5 stable first, B=15 solid fallback, C=40 experimental, D=80 last resort / often broken.
# Format: id:priority:note
TIERS=(
  "21521:5:A ollama-cloud-pro"
  "21523:5:A sensenova-tokenplan"
  "20736:15:B opencode go-1"
  "20752:15:B chybenzun GLM-5.2"
  "21546:40:C nvidia experimental"
  "21522:80:D local-grok-bridge keep out of main path"
  "4753:80:D muyuan often 403/5xx"
  "4638:80:D anyroute responses_unsupported"
  "4940:80:D chybenzun quota/403"
  "4933:80:D newapi unexpected_status"
)

echo "group=${GROUP_NAME} apply=${APPLY}"
echo "---- current openai/apikey accounts ----"
run_psql -c "
SELECT a.id, a.name, a.priority, a.status, a.schedulable,
  left(coalesce(a.credentials->>'base_url',''),48) AS base,
  left(coalesce(a.temp_unschedulable_reason,''),40) AS reason,
  a.extra->>'health_probe_status' AS hp
FROM accounts a
JOIN account_groups ag ON ag.account_id=a.id
JOIN groups g ON g.id=ag.group_id
WHERE g.name='${GROUP_NAME_SQL}' AND a.deleted_at IS NULL AND a.type='apikey'
ORDER BY a.priority NULLS LAST, a.id;
"

echo "---- planned changes ----"
for item in "${TIERS[@]}"; do
  id="${item%%:*}"
  rest="${item#*:}"
  prio="${rest%%:*}"
  note="${rest#*:}"
  cur=$(run_psql -At -c "SELECT COALESCE(priority::text,'null') FROM accounts WHERE id=${id} AND deleted_at IS NULL;" || true)
  if [[ -z "$cur" ]]; then
    echo "SKIP missing id=${id} (${note})"
    continue
  fi
  if [[ "$cur" == "$prio" ]]; then
    echo "KEEP id=${id} priority=${prio} (${note})"
  else
    echo "SET  id=${id} ${cur} -> ${prio} (${note})"
    if [[ "$APPLY" == "1" ]]; then
      run_psql -q -c "
UPDATE accounts
SET priority = ${prio},
    extra = COALESCE(extra, '{}'::jsonb) || jsonb_build_object(
      'priority_tier_note', '$(sql_escape "$note")',
      'priority_tier_updated_at', NOW()
    ),
    updated_at = NOW()
WHERE id = ${id} AND deleted_at IS NULL;
"
    fi
  fi
done

if [[ "$APPLY" != "1" ]]; then
  echo
  echo "dry-run only. re-run with --apply to write."
else
  echo
  echo "applied. current snapshot:"
  run_psql -c "
SELECT a.id, a.name, a.priority, a.schedulable,
  left(coalesce(a.credentials->>'base_url',''),48) AS base
FROM accounts a
JOIN account_groups ag ON ag.account_id=a.id
JOIN groups g ON g.id=ag.group_id
WHERE g.name='${GROUP_NAME_SQL}' AND a.deleted_at IS NULL AND a.type='apikey'
ORDER BY a.priority NULLS LAST, a.id;
"
fi
