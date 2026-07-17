#!/bin/zsh
set -euo pipefail

# Install/update LaunchAgent for mixed+relay health probe.
# Covers gpt-中转 (responses) and 通用 (chat/mixed) by default.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LABEL="local.sub2api-relay-group-health"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_OUT="$HOME/Library/Logs/sub2api-relay-group-health.log"
LOG_ERR="$HOME/Library/Logs/sub2api-relay-group-health.err.log"
PROBE_SH="${REPO_ROOT}/tools/probe_relay_group_health.sh"

GROUP_NAMES_VALUE="${GROUP_NAMES:-gpt-中转,通用}"
# Keep gpt-中转 on responses; 通用 auto resolves to chat via mixed family.
PROBE_MODE_VALUE="${PROBE_MODE:-auto}"
SOFT_PRIORITY_ADJUST_VALUE="${SOFT_PRIORITY_ADJUST:-0}"
HOUR_VALUE="${HOUR:-6}"
MINUTE_VALUE="${MINUTE:-35}"

mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"

cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>GROUP_NAMES</key>
		<string>${GROUP_NAMES_VALUE}</string>
		<key>PROBE_MODE</key>
		<string>${PROBE_MODE_VALUE}</string>
		<key>SOFT_PRIORITY_ADJUST</key>
		<string>${SOFT_PRIORITY_ADJUST_VALUE}</string>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>Label</key>
	<string>${LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/zsh</string>
		<string>${PROBE_SH}</string>
	</array>
	<key>RunAtLoad</key>
	<false/>
	<key>StandardErrorPath</key>
	<string>${LOG_ERR}</string>
	<key>StandardOutPath</key>
	<string>${LOG_OUT}</string>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>${HOUR_VALUE}</integer>
		<key>Minute</key>
		<integer>${MINUTE_VALUE}</integer>
	</dict>
	<key>WorkingDirectory</key>
	<string>${REPO_ROOT}</string>
</dict>
</plist>
PLIST

launchctl bootout "gui/$(id -u)/${LABEL}" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
launchctl enable "gui/$(id -u)/${LABEL}" 2>/dev/null || true
echo "installed ${PLIST}"
echo "groups=${GROUP_NAMES_VALUE} mode=${PROBE_MODE_VALUE} soft_priority=${SOFT_PRIORITY_ADJUST_VALUE} schedule=${HOUR_VALUE}:${MINUTE_VALUE}"
echo "manual run: GROUP_NAMES='${GROUP_NAMES_VALUE}' PROBE_MODE=${PROBE_MODE_VALUE} ${PROBE_SH}"
