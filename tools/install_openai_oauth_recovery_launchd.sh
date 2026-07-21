#!/bin/zsh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROBE_SCRIPT="${SCRIPT_DIR}/probe_openai_oauth_recovery.sh"
RECOVER_SCRIPT="${SCRIPT_DIR}/recover_openai_oauth_error_accounts.sh"
PROBE_PLIST_PATH="${HOME}/Library/LaunchAgents/local.sub2api-openai-oauth-pool-probe.plist"
RECOVER_PLIST_PATH="${HOME}/Library/LaunchAgents/local.sub2api-openai-oauth-error-recover.plist"
LOG_DIR="${HOME}/Library/Logs"
PROBE_STDOUT_LOG="${LOG_DIR}/sub2api-openai-oauth-pool-probe.log"
PROBE_STDERR_LOG="${LOG_DIR}/sub2api-openai-oauth-pool-probe.err.log"
RECOVER_STDOUT_LOG="${LOG_DIR}/sub2api-openai-oauth-error-recover.log"
RECOVER_STDERR_LOG="${LOG_DIR}/sub2api-openai-oauth-error-recover.err.log"
LAUNCHD_DOMAIN="gui/$(id -u)"

reload_plist() {
  local plist_path="$1"
  local label="$2"
  launchctl bootout "${LAUNCHD_DOMAIN}/${label}" >/dev/null 2>&1 || true
  launchctl enable "${LAUNCHD_DOMAIN}/${label}" >/dev/null 2>&1 || true
  launchctl bootstrap "$LAUNCHD_DOMAIN" "$plist_path"
}

mkdir -p "${HOME}/Library/LaunchAgents" "$LOG_DIR"
chmod +x "$PROBE_SCRIPT" "$RECOVER_SCRIPT"

# Probe: active 但待探测/冷却到期的本机账号，API 成功后再放回可调度
cat > "$PROBE_PLIST_PATH" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>local.sub2api-openai-oauth-pool-probe</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/zsh</string>
    <string>${PROBE_SCRIPT}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StartInterval</key>
  <integer>300</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${PROBE_STDOUT_LOG}</string>
  <key>StandardErrorPath</key>
  <string>${PROBE_STDERR_LOG}</string>
</dict>
</plist>
PLIST_EOF

# Recover: error 账号本机 refresh 成功后才释放（error 不进服务内置 refresh 候选）
cat > "$RECOVER_PLIST_PATH" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>local.sub2api-openai-oauth-error-recover</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/zsh</string>
    <string>${RECOVER_SCRIPT}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>MAX_ACCOUNTS</key>
    <string>80</string>
  </dict>
  <key>StartInterval</key>
  <integer>1800</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${RECOVER_STDOUT_LOG}</string>
  <key>StandardErrorPath</key>
  <string>${RECOVER_STDERR_LOG}</string>
</dict>
</plist>
PLIST_EOF

reload_plist "$PROBE_PLIST_PATH" "local.sub2api-openai-oauth-pool-probe"
reload_plist "$RECOVER_PLIST_PATH" "local.sub2api-openai-oauth-error-recover"

echo "Installed and loaded:"
echo "  $PROBE_PLIST_PATH (every 300s)"
echo "  $RECOVER_PLIST_PATH (every 1800s, MAX_ACCOUNTS=80)"
