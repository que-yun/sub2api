#!/usr/bin/env bash
# 内部实现：由仓库根目录 `make deploy` 调用。
# 上传已嵌入前端的 linux 二进制到 VPS standby，并校验 /health 与 /。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LINUX_BIN="${LINUX_BIN:-${ROOT_DIR}/deploy/out/linux-amd64/sub2api}"
REMOTE_EXEC_TARGET="${REMOTE_EXEC_TARGET:-root@100.99.28.61}"
SSH_PORT="${SSH_PORT:-22222}"
SSH_IDENTITY_FILE="${SSH_IDENTITY_FILE:-${HOME}/.ssh/sub2api_vps_db_tunnel_ed25519}"
REMOTE_BIN="${REMOTE_BIN:-/opt/sub2api-standby/sub2api}"
SERVICE_NAME="${SERVICE_NAME:-sub2api-standby.service}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:6781/health}"
PAGE_URL="${PAGE_URL:-http://127.0.0.1:6781/}"
CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-20}"

if [[ ! -x "${LINUX_BIN}" ]]; then
  echo "Missing linux binary: ${LINUX_BIN}" >&2
  echo "Run from repo root: make deploy" >&2
  exit 1
fi

# 拒绝推送非 embed 二进制
if command -v strings >/dev/null 2>&1; then
  if strings "${LINUX_BIN}" | grep -F -q "Frontend not embedded"; then
    echo "Refusing non-embed binary: ${LINUX_BIN}" >&2
    exit 1
  fi
  if ! strings "${LINUX_BIN}" | grep -F -q "__CSP_NONCE_VALUE__"; then
    echo "Refusing binary without frontend embed markers: ${LINUX_BIN}" >&2
    exit 1
  fi
fi

ssh_args=(
  -o BatchMode=yes
  -o ConnectTimeout="${CONNECT_TIMEOUT}"
  -o ServerAliveInterval=10
  -o ServerAliveCountMax=3
  -o StrictHostKeyChecking=accept-new
  -p "${SSH_PORT}"
)
if [[ -n "${SSH_IDENTITY_FILE}" ]]; then
  ssh_args+=(-i "${SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes)
fi

if command -v shasum >/dev/null 2>&1; then
  LOCAL_HASH="$(shasum -a 256 "${LINUX_BIN}" | awk '{print $1}')"
else
  LOCAL_HASH="$(sha256sum "${LINUX_BIN}" | awk '{print $1}')"
fi
echo "Deploying ${LINUX_BIN} (${LOCAL_HASH}) -> ${REMOTE_EXEC_TARGET}:${REMOTE_BIN}"

gzip -c "${LINUX_BIN}" | ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" \
  "gzip -dc > /tmp/sub2api-new && test -s /tmp/sub2api-new && sha256sum /tmp/sub2api-new"

ssh "${ssh_args[@]}" "${REMOTE_EXEC_TARGET}" "set -euo pipefail
REMOTE_HASH=\$(sha256sum /tmp/sub2api-new | awk '{print \$1}')
if [[ \"\${REMOTE_HASH}\" != \"${LOCAL_HASH}\" ]]; then
  echo \"Hash mismatch: remote=\${REMOTE_HASH} local=${LOCAL_HASH}\" >&2
  exit 1
fi
ts=\$(date +%Y%m%d-%H%M%S)
if [[ -f '${REMOTE_BIN}' ]]; then
  cp -a '${REMOTE_BIN}' \"${REMOTE_BIN}.bak-before-deploy-\${ts}\"
fi
systemctl stop '${SERVICE_NAME}' || true
install -m 0755 /tmp/sub2api-new '${REMOTE_BIN}'
rm -f /tmp/sub2api-new
systemctl start '${SERVICE_NAME}'
for i in 1 2 3 4 5 6 7 8 9 10; do
  if systemctl is-active --quiet '${SERVICE_NAME}'; then
    break
  fi
  sleep 1
done
systemctl is-active '${SERVICE_NAME}'
sha256sum '${REMOTE_BIN}'

curl -fsS --max-time 5 '${HEALTH_URL}' >/tmp/sub2api-health.out
echo \"health=\$(cat /tmp/sub2api-health.out)\"

code=\$(curl -sS -o /tmp/sub2api-page.out -w '%{http_code}' --max-time 5 '${PAGE_URL}')
head=\$(head -c 120 /tmp/sub2api-page.out | tr '\\n' ' ')
echo \"page_http=\${code} body=\${head}\"
if [[ \"\${code}\" != \"200\" ]]; then
  echo \"Frontend page check failed: HTTP \${code}\" >&2
  exit 1
fi
if ! grep -qiE '<!doctype html>|<html' /tmp/sub2api-page.out; then
  echo \"Frontend page check failed: response is not HTML\" >&2
  exit 1
fi
if grep -Fq '404 page not found' /tmp/sub2api-page.out; then
  echo \"Frontend page check failed: still 404 page not found\" >&2
  exit 1
fi
if grep -Fq 'Frontend not embedded' /tmp/sub2api-page.out; then
  echo \"Frontend page check failed: binary still non-embed\" >&2
  exit 1
fi
echo 'VPS binary deploy + frontend verify OK'
"

echo "Deploy completed."
