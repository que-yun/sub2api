# Grok 上游请求缓存对照（2026-07-21）

## 结论

Sub2API 已实现 xAI 上游 prompt cache 的关键机制，并且真实流量已命中。它并不把模型响应缓存在本地 Redis，而是为每个 Grok 请求写入稳定、按 API key 和上游模型隔离的 `prompt_cache_key`，由 xAI 复用提示词前缀。

最近 72 小时的 Grok 用量记录：

- 7,812 个请求中，5,838 个返回 `cache_read_tokens`，请求命中率 74.73%。
- 完整提示词 token（`input + cache_creation + cache_read`）为 600,921,973，其中 445,763,840 为缓存读，token 命中占比 74.18%。

最近 24 小时请求命中率为 60.38%，cache-read token 占完整提示词 token 的 66.42%。因此该缓存链路正在工作，不是 Redis 调度缓存故障。

## 与 grok2api 的差异

`chenyme/grok2api` 有两层能力：

1. 生成稳定会话身份并传入上游的 `prompt_cache_key`。
2. 启用时在本地内存或 Redis 保存上一轮 `reasoning.encrypted_content`，下一轮自动注入。默认 TTL 为 1 小时，Redis 命中会续期。

当前 Sub2API 已覆盖第一层，且更严格：缓存身份由 API key ID、上游模型和会话/稳定前缀派生，原始客户端 session ID 不会直接传给 xAI，跨租户不会共享。对于 Grok Free OAuth，它还会在安全条件下追加原生 `web_search` / `x_search` 路由，以避免落到不可缓存的 build-free 路径。

当前 Sub2API 没有 grok2api 第二层的服务端 reasoning replay。Claude Code 等客户端若能把上游返回的 thinking signature / `encrypted_content` 原样带到下一轮，Sub2API 已可转换并透传，不需要 replay；客户端会丢弃该内容且又不使用 `previous_response_id` 时，才会缺少 grok2api 的自动补偿。

## 建议

当前不建议为了缓存命中率重做 `prompt_cache_key`，它已在有效工作。优先拿一段实际客户端多轮请求确认两件事：

1. 连续轮次的上游请求是否携带同一个生成后的 `prompt_cache_key`。
2. 下一轮是否保留 reasoning signature / `encrypted_content`，或使用 `previous_response_id`。

只有第二项确认缺失并导致 Grok 多轮推理失败或明显重复计算时，才值得按 grok2api 的模式增加带 TTL、容量上限和租户隔离的 reasoning replay；不要缓存完整模型响应。

## grok2api 最近优化

截至 `v3.0.7`（2026-07-21），最近与网关运行质量相关的提交如下：

| 提交 | 优化 | 作用 |
| --- | --- | --- |
| `48ec7dc` | prompt-cache session affinity | 优先识别 Claude Code / Codex 的会话信号，缓存身份加入模型隔离；无显式会话时用 system + 首条 user 的稳定前缀兜底，避免每轮换 key。 |
| `67133a9` | 清理缓存兼容代码 | 删除重复 header / 工具判断，收窄缓存路由条件，降低错误复用风险。 |
| `fa13c08` | reasoning recovery 保留 429 | 解密失败后的无状态恢复若遇 429，仍将限流返回调度器，避免错误地继续使用已限流账号。 |
| `8f9c9f3` | Anthropic web-search 与 reasoning replay 隔离 | web-search 轮次不读取或覆盖普通对话的 encrypted reasoning，随后普通轮次仍可正确回放。 |
| `505c0b3` | 代理池故障隔离 | 共享代理池的一次 403、5xx 或上游业务错误不再把整个出口节点置全局冷却；仅连接层错误允许有限重试。 |
| `5190c7b` | Free 账单画像推断 | 账单快照未标计划但所有余额与用量字段均为零时推断为 Free，同时保守排除非零付费信号，改善 Free 账号的缓存路由选择。 |

其中前四项直接关联多轮 Grok 请求；后两项提高账号池和出口在错误流量下的可用性。

## 验证

- 默认服务层测试 `go test -count=1 ./internal/service` 已通过。
- 带 `unit` tag 的服务层测试 `go test -tags=unit -count=1 ./internal/service` 已通过。此前测试引用了未定义的旧配额常量，现已改为 `xai.GrokFreeRolling24hTokenLimit`。
