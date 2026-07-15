#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LABEL="local.sub2api-grok-recover-probe"
SRC_PLIST="${SCRIPT_DIR}/local.sub2api-grok-recover-probe.plist"
DST_PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"

if [[ ! -f "${SRC_PLIST}" ]]; then
  echo "missing ${SRC_PLIST}" >&2
  exit 1
fi
if [[ ! -x "${REPO_ROOT}/tools/run_grok_recover_probe.sh" ]]; then
  echo "missing executable ${REPO_ROOT}/tools/run_grok_recover_probe.sh" >&2
  exit 1
fi

mkdir -p "${HOME}/Library/LaunchAgents"
cp "${SRC_PLIST}" "${DST_PLIST}"

# reload
launchctl bootout "gui/$(id -u)/${LABEL}" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$(id -u)" "${DST_PLIST}"
launchctl enable "gui/$(id -u)/${LABEL}" >/dev/null 2>&1 || true

echo "Installed ${DST_PLIST}"
echo "Interval: every 1800s (fast queues each tick; sticky 403 every 6h inside script)"
echo "Logs:"
echo "  ~/Library/Logs/sub2api-grok-recover-probe.log"
echo "  ~/Library/Logs/sub2api-grok-recover-probe.err.log"
echo "Manual run:"
echo "  ${REPO_ROOT}/tools/run_grok_recover_probe.sh"
echo "Force sticky 403 queue once:"
echo "  GROK_RECOVER_FORCE_403=1 ${REPO_ROOT}/tools/run_grok_recover_probe.sh"
