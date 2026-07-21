# Grok 账号 20768 重新授权前快照

记录时间：2026-07-12 12:51:33 +0800

## 账号

- ID：20768
- 名称：DuketaAryal@hotmail.com
- 平台：grok
- 类型：oauth
- 状态：active
- 错误信息：空
- 临时暂停到：2026-07-12 13:02:11.244803 +0800
- base_url：https://api.x.ai/v1
- email：DuketaAryal@hotmail.com
- client_id：b1a00492-073a-47ea-816f-4c329264a828
- access_token：存在，未记录明文
- refresh_token：存在，未记录明文
- expires_at：2026-07-12T08:21:07Z
- scope：openid profile email offline_access grok-cli:access api:access

## sub2api 中的旧 quota 快照

- status_code：200
- updated_at：2026-07-12T02:21:08Z
- last_headers_seen_at：2026-07-12T02:21:08Z
- requests.limit：8300
- requests.remaining：8300
- tokens.limit：53000000
- tokens.remaining：53000000

说明：这个快照是旧成功状态，不能代表当前可用性。

## 重新授权前真实探测

用当前 access_token 和 Grok Build header 探测：

- `https://api.x.ai/v1/responses`：403
- `https://cli-chat-proxy.grok.com/v1/responses`：403

用 refresh_token 换新 access_token：

- token refresh：200
- 新 access_token 打 `https://api.x.ai/v1/responses`：403
- 新 access_token 打 `https://cli-chat-proxy.grok.com/v1/responses`：403

模型交叉验证：

- `grok-4.5`：403
- `grok-4.3`：403
- `grok-build-0.1`：403
- `grok-build-latest`：403

上游错误：

```json
{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please log into console.x.ai and update the permissions, or contact support."}
```

## 当前判断

重新授权前，该账号的 refresh token 可用，但新旧 access token 都没有 chat endpoint 权限。网页侧能看到历史记录不等价于 OAuth/API chat endpoint 可用。

重新授权后重点对比：

- access_token / expires_at 是否更新
- base_url 是否改成预期值
- `/responses` 是否从 403 变成 200 或 429
- 如果仍是 `permission-denied`，基本可判定为账号 endpoint entitlement 问题
