#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SCRIPT="${SCRIPT_DIR}/run_gzip_front_proxy_launchd.sh"
PLIST_PATH="${HOME}/Library/LaunchAgents/local.sub2api-gzip-front-proxy.plist"
LOG_DIR="${HOME}/Library/Logs"
STDOUT_LOG="${LOG_DIR}/sub2api-gzip-front-proxy.stdout.log"
STDERR_LOG="${LOG_DIR}/sub2api-gzip-front-proxy.stderr.log"

mkdir -p "${HOME}/Library/LaunchAgents" "${LOG_DIR}"
chmod +x "${RUN_SCRIPT}"

cat > "${PLIST_PATH}" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>local.sub2api-gzip-front-proxy</string>

  <key>ProgramArguments</key>
  <array>
    <string>/bin/zsh</string>
    <string>${RUN_SCRIPT}</string>
  </array>

  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/local/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>

  <key>WorkingDirectory</key>
  <string>${SCRIPT_DIR}/..</string>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <true/>

  <key>ThrottleInterval</key>
  <integer>5</integer>

  <key>StandardOutPath</key>
  <string>${STDOUT_LOG}</string>

  <key>StandardErrorPath</key>
  <string>${STDERR_LOG}</string>
</dict>
</plist>
PLIST_EOF

launchctl unload "${PLIST_PATH}" >/dev/null 2>&1 || true
launchctl load "${PLIST_PATH}"

echo "Installed launchd job at ${PLIST_PATH}"
echo "stdout log: ${STDOUT_LOG}"
echo "stderr log: ${STDERR_LOG}"
