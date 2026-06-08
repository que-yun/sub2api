# failsafebreaker 接线说明(等 WIP 收口后照此接进 gpt-中转 真实链路)

目标:把组内选路/失败转移换成「健康分级 + Route 失败转移 + panic 兜底」,修复
①一直打报错渠道 ②降级/熔断混淆 ③冷却导致完全不可用。**先只灰度 gpt-中转(group 24)。**

## 前置
- 等你/codex 的 `codex/upgrade-upstream` WIP 提交收口(它在改 openai_account_scheduler / runtime_block_fastpath / chat_completions)。
- 本包在 `internal/pkg/failsafebreaker`(+ `internal/pkg/failclass`),纯新增、已测试,不依赖请求路径。

## 三个接入点(按真实代码定位)

### 1) 选号:替换 priority+round-robin
- 现状:`gateway_service.go: SelectAccountWithLoadAwareness(1523)` → `tryAcquireByLegacyOrder(2177)` → `sortAccountsByPriorityAndLastUsed(2942)`(只按优先级+最近最少用,无健康分级)。
- 改:候选账号 IDs 交给 `Registry.PickBest(ids, stickyAccountID)` 选 Healthy→Degraded→panic;或直接用 `Route(ids, sticky, call)` 把"选+调+失败转移"一把做完(推荐)。
- 粘性:把现有 sticky session 的 accountID 作为 `preferred` 传入(Healthy 才粘住)。

### 2) 失败转移循环:换成 Route
- 现状:各 handler 自己写循环(`openai_images.go: for attempt<8`、handler 里 `excludedIDs` 重选)。
- 改:用 `Route(groupAccountIDs, sticky, func(accountID) (status, body, err){ 真实上游调用 })`。
  Route 内部保证:同请求内排除已失败号(绝不重复打)、Healthy→Degraded→panic 顺序、panic 绕过冷却强制试。

### 3) 记账 + 冷却:用真实 reset 时间(修你说的冷却 bug 的根)
- 现状:`openai_account_runtime_block_fastpath.go: handleOpenAIAPIKey5xxCooldown / isOpenAIGroupModelCircuitOpen`,`ratelimit_service.go: HandleUpstreamError / calculateOpenAI429ResetTime`。
- 改:每次上游返回后
  `res := failclass.Classify(status, err, body, retryAfter)` —— **retryAfter 传 `calculateOpenAI429ResetTime` 的真实 reset**,
  这样配额冷却=精确到 reset(不是固定 10/30min 猜),避免"冷却过长把还能用的号闷死"。
  然后 `reg.Record(accountID, res)`(若没用 Route 的话;用 Route 则它内部已记)。
- 你已有的领域分类(`classifyOpenAIAPIKeyRequestError` 等)可保留,把结论映射成 failclass 类别;
  或直接让 failclass.Classify 吃 status+body+err。

## 冷却语义(必须守住,正是你报的 bug)
- transient/5xx:`OpenDelay` 给**短值**(秒级),半开**被动**靠真实流量恢复;**别用 10/30min 硬冷却**。
- 配额:冷却到**真实 reset**(retryAfter),reset 前不探测、不重试。
- 死key(401/403):disabled,成功才恢复。
- **没有更好选择时**:Route 的 panic 兜底**绕过冷却强制试**(forceExecute),冷却永不等于"彻底不可用"。

## Feature flag + 灰度
- 加 `GATEWAY_FAILSAFE_ROUTING`(env/config),默认关。
- 先**只对 group 24(gpt-中转)**开;跑稳再扩。
- 建议先 **shadow 模式**:Route/PickBest 只计算并**日志输出"会选哪个/会跳过哪个"**,实际仍走旧逻辑;对比日志确认无误,再切真路由。

## 验证
- `go test ./internal/pkg/failsafebreaker/ ./internal/pkg/failclass/`(本包,已绿)。
- 接入后跑全量 `make test`(53 个弹性测试)。
- 上线后看日志:`circuit_ignored_without_fallback` 应消失(改为组内 Route 消化);同一账号连续报错(如之前 account_id:4635 连 123 次)应不再出现。

## 可观测
- `Registry.Snapshot()` → 每账号三档,挂到 admin 端点,直接看 gpt-中转 17 个号实时 healthy/degraded/broken。

## 精确接入点(读 WIP 收口后代码定位,2026-06-09)

> 重要:codex WIP 已在以下文件构建"加权选号 + OpenAIGroupModelRuntimeCircuit + 分类/冷却",
> failsafebreaker 与之**高度重叠**。**实际接线务必等 codex scheduler 收口后再做**,否则撞车+重复。
> 接线前先决策:用 failsafe-go 换掉手写内核(推荐,你之前要的"用成熟组件"),还是保留 codex 手写版。

三处真实 seam:

1. **选号** `internal/service/openai_account_scheduler.go:377 defaultOpenAIAccountScheduler.Select`
   - 现状:`buildOpenAIWeightedSelectionOrder`(加权)+ session-hash + RNG。
   - 接法:把候选账号先按 `failsafebreaker.Tier` 分档(Healthy>Degraded>Broken),
     在 Healthy 档内再用现有加权/session-hash 选;无 Healthy 降 Degraded;全 Broken 走 PickBest 的 panic。
     即 failsafebreaker 管"档位 + 兜底",codex 的加权管"同档内挑哪个"。

2. **失败转移循环**(handler 级:select→ForwardAsChatCompletions→失败转移)
   - 现状:`openai_gateway_chat_completions{,_raw}.go` 单次调用返回 `UpstreamFailoverError`;handler 排除重选重试。
   - 接法:用 `failsafebreaker.Route(groupAccountIDs, sticky, call)` 替换 handler 那层循环,
     `call(accountID)` 内部调 `ForwardAsChatCompletions`。Route 保证同请求不重打失败号 + 全坏 panic 绕冷却硬试。

3. **记账 + 冷却**(关键:修"冷却过长闷死降级号")
   - 上游调用点:`httpUpstream.Do(...)`(_raw.go:167 / .go:225)之后。
   - 接法:`res := failclass.Classify(status, err, body, retryAfter)`,
     **retryAfter 传 `calculateOpenAI429ResetTime` 的真实 reset**(配额冷却=精确到 reset,不固定 10/30min);
     再 `reg.Record(accountID, res)`(用 Route 则内部已记)。
   - 用 failsafebreaker 的话,可让它**取代手写的 OpenAIGroupModelRuntimeCircuit**(per-account 断路器 + 三档)。

Flag:`GATEWAY_FAILSAFE_ROUTING`,默认关,先只对 group 24(gpt-中转)开 + shadow 对比。
