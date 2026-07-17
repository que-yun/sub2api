# 混合账户组（稳定 + 速度）最佳实践

面向 `通用` 这种「多来源 OpenAI 兼容 apikey 混池」。目标不是理论最优，而是接近内置冷却效果，并复用你已有的 `probe_relay_group_health` 能力。

## 1. 先认清三层职责

| 层 | 谁负责 | 作用 | 不负责 |
|---|---|---|---|
| A. 请求时止血 | sub2api 内置 | 401/403/429/5xx → `temp_unschedulable_*`，失败换号 | 长期排序 |
| B. 定时探活 | 你的 health probe + 可选 Scheduled Test | 提前发现坏号、写 `health_probe_*`、短冷却 | 精细模型路由 |
| C. 人工/半自动分档 | priority 档位 | 稳定源优先、实验源垫后 | 实时限流细节 |

openai 分组（如 `通用`）主排序：

```text
priority 越小越先
→ 同档内 load / last-used / shuffle
→ 请求失败再冷却并 failover
```

`model_routing` 对 anthropic 更有效；openai 混池不要只靠 routing 排序。

## 2. 推荐档位（priority）

数字是建议值，可按你实际体感微调，但**档距要拉开**（同档才靠 shuffle 分担）。

| 档 | priority | 放什么 | 例子（当前 通用） |
|---|---:|---|---|
| A 主路径 | 5 | 稳定 + 相对快、额度靠谱 | `21521` ollama-cloud-pro, `21523` sensenova |
| B 备援 | 15 | 能打但偶发限流/慢 | `20736` opencode, `20752` chybenzun GLM |
| C 实验 | 40 | 新源/免费/模型全但稳性未知 | `21546` NVIDIA |
| D 兜底或隔离 | 80 | 常 403/额度/不支持关键协议 | `4753` muyuan, `4638` anyroute, `4940` chybenzun, `21522` local-grok-bridge |

原则：

1. **不要**把实验源和本地 bridge 放 priority=1。
2. 长期坏号优先 `schedulable=false` 或踢出主组，而不是继续堆在 A/B。
3. 同能力账号放同档，让内置 shuffle 自然分摊。
4. 模型特化账号（只适合 GLM/某厂商）可以留在组里，但 priority 放 B/C，真正精准再靠 mapping/routing 辅助。

应用脚本：

```bash
# 先看差异
/Users/yunque/services/sub2api-upstream-custom/tools/apply_mixed_group_priority.sh

# 确认后写入
/Users/yunque/services/sub2api-upstream-custom/tools/apply_mixed_group_priority.sh --apply
```

## 3. 定时轮询：内置 + 手写增强怎么叠

### 3.1 内置已有

- 请求失败冷却（`temp_unschedulable`、429/overload 设置）
- 账号 Scheduled Test（cron + `auto_recover`）：测活成功可清 error/rate-limit
  - 路径：`POST /admin/scheduled-test-plans`，`GET /admin/accounts/:id/scheduled-test-plans`
- Grok / OAuth 另有专用 recover、sync 任务

内置**没有**：按成功率/延迟自动重算 priority。

### 3.2 你手写的 probe（已增强）

脚本：`tools/probe_relay_group_health.sh`

增强点：

1. **多分组**：`GROUP_NAMES="gpt-中转,通用"`
2. **探测模式**：
   - `responses`：适合 codex/gpt 中转
   - `chat`：适合 通用/NVIDIA/ollama 等 OpenAI chat 兼容
   - `auto`：gpt/codex 组走 responses，其余走 chat；responses 不支持时回落 chat
3. **结果落库**：`extra.health_probe_*` + `account_health_probe_results`
4. **按类别冷却**：
   - site_blocked 30m
   - temporary_5xx / request_error 10m
   - auth/rate_limit 15m
   - model/responses unsupported 120m
5. **可选软降权**：`SOFT_PRIORITY_ADJUST=1` 时，对 degraded/unavailable 在 baseline 上抬 priority（默认关，避免和手动分档打架）

安装/更新定时任务：

```bash
# 默认：每天 06:35 扫 gpt-中转 + 通用
/Users/yunque/services/sub2api-upstream-custom/tools/install_relay_group_health_launchd.sh

# 若要开启软降权（谨慎）
SOFT_PRIORITY_ADJUST=1 /Users/yunque/services/sub2api-upstream-custom/tools/install_relay_group_health_launchd.sh
```

手动只扫通用：

```bash
GROUP_NAMES="通用" PROBE_MODE=chat PROBE_SLEEP_SECONDS=5 \
  /Users/yunque/services/sub2api-upstream-custom/tools/probe_relay_group_health.sh
```

### 3.3 推荐叠加（接近“自动运维”但不乱跳）

```text
手动/脚本分档 (priority A/B/C/D)
        +
每天 probe 预判坏号 (temp_unschedulable + health_probe_*)
        +
请求时内置冷却/换号
        +
关键账号 Scheduled Test + auto_recover（可选）
```

不要做的事：

- 每小时大幅改 priority（抖动大）
- 把所有账号 priority 打成 1
- 用 model_routing 替代 openai 混池分档
- 坏了很久还留在 A 档只靠 cooldown

## 4. 故障分级处理

| 现象 | 动作 | 时长/档位 |
|---|---|---|
| 偶发 429/5xx | 内置/probe 冷却即可 | 10–15 分钟 |
| 额度不足 / 明确 403 渠道禁用 | 冷却 + 降到 D 或 `schedulable=false` | 直到人工恢复 |
| 协议不支持（responses 404） | 长冷却；若只是 responses 问题，通用组用 chat 探测 | D 或移出 gpt 中转组 |
| 新实验源（NVIDIA 等） | 先 C 档 | 观察 1–3 天再升 B |
| 本地 bridge / 自指 | 默认 D 或独立组 | 避免抢主流量 |

## 5. 日常检查清单

1. 看 `通用` apikey 列表：priority 是否仍分档清晰  
2. 看最近 `health_probe_status` / `temp_unschedulable_reason`  
3. 日志：`~/Library/Logs/sub2api-relay-group-health.log`  
4. 主路径 A 档是否至少 1 个 available  
5. 实验源是否误升到 1/5

SQL 快照：

```sql
SELECT a.id, a.name, a.priority, a.schedulable,
       a.extra->>'health_probe_status' AS hp,
       a.extra->>'health_probe_category' AS cat,
       a.temp_unschedulable_reason
FROM accounts a
JOIN account_groups ag ON ag.account_id=a.id
JOIN groups g ON g.id=ag.group_id
WHERE g.name='通用' AND a.deleted_at IS NULL AND a.type='apikey'
ORDER BY a.priority, a.id;
```

## 6. 和“完全自动重排”的边界

当前最佳实践有意停在：

- **分档固定**
- **坏号临时踢出**
- **好号自然回到可调度**

而不是根据成功率重写 priority。原因：混池样本噪声大，自动重排容易把偶发抖动放大成主路径抖动。

若以后要增强，优先顺序建议：

1. probe 覆盖更多组（已做）
2. chat/responses 自适应（已做）
3. 连续 N 次 unavailable → 建议降档报告（半自动）
4. 最后才考虑受限的 soft priority adjust

## 7. 相关文件

- 探针：`tools/probe_relay_group_health.sh`
- 分档：`tools/apply_mixed_group_priority.sh`
- 安装定时：`tools/install_relay_group_health_launchd.sh`
- LaunchAgent：`~/Library/LaunchAgents/local.sub2api-relay-group-health.plist`
- 日志：`~/Library/Logs/sub2api-relay-group-health.log`


## 8. 当前落地状态（2026-07-16）

已落地：

1. `通用` priority 分档已应用：
   - A(5): 21521 ollama-cloud-pro, 21523 sensenova
   - B(15): 20736 opencode, 20752 chybenzun GLM
   - C(40): 21546 NVIDIA
   - D(80): 4753/4638/4940/4933/21522
2. health probe 已增强：`GROUP_NAMES` 多组、`chat/responses/auto`、JSON 账户读取、模型优选/重试、可选 soft priority
3. LaunchAgent 已更新为每天 06:35 扫描 `gpt-中转,通用`
4. 冒烟结果（chat 模式，2026-07-16）：
   - available：21521 ollama、21523 sensenova、21546 NVIDIA、21522 local-grok-bridge
   - B 档波动：20736 偶发 timeout；20752 chybenzun GLM 仍常 404
   - D 档常坏：4638/4753/4933/4940 多为 5xx/302/404
   - NVIDIA 需用真实 chat 模型（如 `meta/llama-3.1-8b-instruct`），探针已固化该默认

建议保持：

- `SOFT_PRIORITY_ADJUST=0`（手动分档为主）
- 每天看日志，不必每小时重排 priority
