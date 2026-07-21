#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LABEL="local.sub2api-openai-egress-monitor"
SRC_PLIST="${SCRIPT_DIR}/local.sub2api-openai-egress-monitor.plist"
DST_PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
MONITOR_SH="${REPO_ROOT}/tools/monitor_openai_egress_errors.sh"

if [[ ! -f "${SRC_PLIST}" ]]; then
  echo "missing ${SRC_PLIST}" >&2
  exit 1
fi
if [[ ! -x "${MONITOR_SH}" ]]; then
  echo "missing executable ${MONITOR_SH}" >&2
  exit 1
fi

mkdir -p "${HOME}/Library/LaunchAgents" "${HOME}/Library/Logs"
cp "${SRC_PLIST}" "${DST_PLIST}"

UID_NUM="$(id -u)"
launchctl bootout "gui/${UID_NUM}/${LABEL}" >/dev/null 2>&1 || true
launchctl bootstrap "gui/${UID_NUM}" "${DST_PLIST}"
launchctl enable "gui/${UID_NUM}/${LABEL}" >/dev/null 2>&1 || true

echo "Installed ${DST_PLIST}"
echo "Interval: every 180s (alert-only, no auto node switch)"
echo "Logs:"
echo "  ~/Library/Logs/sub2api-openai-egress-monitor.log"
echo "  ~/Library/Logs/sub2api-openai-egress-monitor.out.log"
echo "  ~/Library/Logs/sub2api-openai-egress-monitor.err.log"
echo "State:"
echo "  ${REPO_ROOT}/deploy/data-host/openai-egress-monitor/"
echo "Manual run:"
echo "  ${MONITOR_SH}"
echo "Dry wider window:"
echo "  WINDOW_MINUTES=60 ${MONITOR_SH}"
