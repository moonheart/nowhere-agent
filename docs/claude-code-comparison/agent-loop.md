# agent-loop vs Claude Code

> 我们的能力:`internal/agent/loop.go`(think→tool→think 循环)。
> CC 对应:`src/query.ts`(主循环)+ `src/QueryEngine.ts` + `src/services/tools/*` + `src/services/api/withRetry.ts`。

## 我们的现状

- **循环结构**(`internal/agent/loop.go:84-146`):固定上限 `MaxIterations=25` 的 `for` 循环;每轮把 `append(history, produced...)` 组装成 `provider.Request` 调 `provider.Adapter.Stream`,用 `consume` 把事件流收敛成一条 assistant 消息 + tool calls;无 tool call 即 `KindDone` 退出。
- **工具执行**(`loop.go:231-233` → `toolruntime.Registry.CallAll`):一轮内所有 tool call 并发执行,结果按顺序打包成单条 user-role `tool_result` 消息回灌;循环外同步等待整批完成才进下一轮。
- **取消**(`loop.go:89-92, 111-114, 160-162`):仅靠 `ctx.Err()`/`ctx.Done()`,在迭代间与流中段检查;取消即 `KindCancelled`,**无优雅排空**(in-flight 工具的合成结果)。
- **错误处理**(`loop.go:104-107, 115-117, 184-185`):`Stream` 出错或流内 `EventError` 直接 `KindError` 并 `return`;**无重试、无 max_tokens 升级、无 malformed tool call 兜底**(JSON 解析失败静默丢 `ToolInput`,仍 dispatch,`loop.go:273-278`)。
- **事件**:`Emitter.Emit(ctx, kind, payload)` 同步推送 `text/thinking/tool_use/tool_result/error/done/cancelled`,由 session runtime 持久化 + 扇出。

## Claude Code 的做法

核心是一个 **`query` 异步生成器**(`src/query.ts:223`)包着 `queryLoop`(`query.ts:245`),内部是 `while(true)` 无限循环,靠一个可变 `State` 结构在迭代间传递,用 7 处 `continue` 站点决定下一轮走向。每个 `continue` 都带 `transition.reason`,形成显式状态机。

```
user turn
   │
   ▼
┌─────────────────────────────────────────────┐
│ while(true) {                               │
│  1. 上下文压缩管线 (snip→micro→collapse→    │
│     autoCompact)         query.ts:405-548   │
│  2. blocking-limit 预检  query.ts:633-653   │
│  3. for await callModel(...) 流式消费       │
│     ├─ 收集 tool_use → toolUseBlocks        │
│     └─ StreamingToolExecutor 边流边执行     │
│                        query.ts:664-878     │
│  4. abort? → 排空合成结果 query.ts:1030-1067│
│  5. needsFollowUp==false?                   │
│     ├─ 413/media 恢复  query.ts:1100-1198   │
│     ├─ max_output_tokens 升级/恢复          │
│     │                    query.ts:1203-1271 │
│     └─ stop hooks → return completed        │
│  6. 执行工具批 (streaming or runTools)      │
│                        query.ts:1395-1424   │
│  7. 注入 attachments/排队消息/记忆/技能     │
│                        query.ts:1598-1662   │
│  8. maxTurns? → return   query.ts:1725-1732 │
│  9. state = {messages+asst+results}; 继续   │
│ }                                           │
└─────────────────────────────────────────────┘
```

1. **循环结构与停止条件**(`query.ts:311 while(true)`)。**无固定迭代上限**,而是多个独立退出理由:`needsFollowUp`(流中是否出现 tool_use,`query.ts:563, 846-849`)是唯一主退出信号——没有 tool_use 且通过 stop hooks 即 `return {reason:'completed'}`(`query.ts:1372`)。`maxTurns` 是软上限,每次工具结果后检查(`query.ts:1725-1732`),到达则 yield `max_turns_reached` 并 `return {reason:'max_turns'}`。**停止决策是「无 tool call」而非「迭代计数」;「继续」被建模成一组原因(next_turn / max_output_tokens_recovery / reactive_compact_retry / stop_hook_blocking…),而非单纯计数。**

2. **工具执行:并发 + 流式两种模式**。
   - tool_use 与结果通过 `id` 配对;结果以 `tool_result`(带 `tool_use_id`)的 user 消息回灌(`query.ts:1399-1424`, `:1736`)。
   - **StreamingToolExecutor**(`services/tools/StreamingToolExecutor.ts:40`):流式期间一旦收到完整 `tool_use` block 就 `addTool` 立即开始执行,模型还在产出后续 block。并发规则:`isConcurrencySafe` 的工具可并行,非安全的独占串行(`canExecuteTool` `:129-135`);结果按收到顺序缓冲、按序 emit。
   - **runTools / 非流式**(`services/tools/toolOrchestration.ts:19`):按 `isConcurrencySafe` 分区(`partitionToolCalls:91`),只读安全批并发(上限默认 10),非安全批串行(`runToolsSerially:118`)。
   - 每个工具调用 `runToolUse`(`toolExecution.ts:339`):先 zod 校验输入(`:617`),跑 PreToolUse hooks、权限判定 `canUseTool`、`tool.call`,再 PostToolUse hooks;**任何一步失败都产出 `is_error:true` 的 `tool_result`,绝不抛出中断循环**。

3. **中断/取消:AbortController 树**。`toolUseContext.abortController` 是根;`StreamingToolExecutor` 持有 `siblingAbortController`(child),每个工具再有自己的 child。**关键机制是「优雅排空」**:abort 后循环不直接退出,而是 `getRemainingResults()` 让每个未完成的工具产出合成 `tool_result`(`query.ts:1030-1044`),保证每个 `tool_use` 都有配对的 `tool_result`(API 强约束)。Bash 出错会 `siblingAbortController.abort('sibling_error')` 级联杀掉兄弟子进程。

4. **转向/插话(steering)**:通过 `abort('interrupt')`(带 reason)。REPL 在用户提交新消息且当前工具全部可取消时 `abort('interrupt')`(`handlePromptSubmit.ts:321-331`),同时把新消息 enqueue 进队列。`reason==='interrupt'` 走特殊路径:循环跳过「[Request interrupted by user]」消息,因为排队的用户消息会作为 attachment 在下一轮注入(`query.ts:1588-1608`)。**所以插话 = 中断当前 turn + 把新指令作为下一轮上下文注入。**

5. **错误处理:多层恢复**。
   - **API 重试**:`withRetry`(`services/api/withRetry.ts:172`)指数退避 + jitter(base 500ms、cap 32s),默认 10 次;按状态码/错误类型决定可重试;529 过载连续 3 次可切 fallback 模型;401/403 刷新 OAuth token 后重建 client。
   - **max_tokens 截断**:流内把 `max_output_tokens` 错误**暂扣**(`isWithheldMaxOutputTokens` `query.ts:178`),先尝试一次性升级到 64k 重试(`:1214-1236`),再退化为多轮「继续」恢复,最多 3 次,耗尽才 surface。
   - **prompt-too-long / 媒体过大**:同样暂扣(`:799-837`),走 collapse drain → reactive compact 恢复(`:1100-1198`)。
   - **malformed tool call**:zod 校验失败产出 `InputValidationError` 的 `tool_result`(`toolExecution.ts:617-682`),连同「schema not sent」提示喂回模型自我纠正;未知工具产出 `No such tool available`(`:398-412`)。**全部建模为 tool_result,不是致命错误。**
   - 循环级 catch(`:970-1012`):用 `yieldMissingToolResultBlocks`(`:125`)为已发出但未配对的 `tool_use` 补合成 `tool_result`,再 yield API 错误消息。

6. **事件发射:async generator**。`query` 是 `AsyncGenerator<StreamEvent|Message|...|Terminal>`(`query.ts:223-232`),`yield` 各类消息。`QueryEngine.submitMessage` 再 `for await` 消费,映射成 `SDKMessage` 并持久化 transcript。

7. **子代理/fork**:`AgentTool` 通过 `runForkedAgent`(`utils/forkedAgent.ts:494`)生成子代理——**复用同一个 `query` 生成器**,但用隔离的 `toolUseContext`(独立 `agentId`、`parentAgentId`、独立 abortController、`querySource`)。子代理消息写入 sidechain transcript。有嵌套深度防护(fork 内禁用 fork)。

> CLI/REPL 专属(不可移植):ESC 键监听、ink/React 渲染、权限对话框交互、queued_command 回放、`isNonInteractiveSession` 区分。`runTools`/`StreamingToolExecutor`/`withRetry`/`withheld+recovery`/`abort 排空` 与 UI 解耦,**可移植**。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 循环上限 | `while(true)`,`maxTurns` 软上限,退出靠「无 tool_use」 | `for < MaxIterations=25` 硬计数,超了报错 | 区分「完成」与「达上限」,达上限不应当成 error |
| 停止信号 | `needsFollowUp`(本轮有无 tool_use) | `len(toolCalls)==0` | 已一致 |
| 工具并发模型 | 按 `isConcurrencySafe` 分区:安全批并发(cap 10)/非安全串行 | 整批无差别并发 | 给工具加 `ConcurrencySafe()` 标记,写工具串行 |
| 流式工具执行 | 边收 tool_use 边执行(StreamingToolExecutor) | 等整轮流结束才 dispatch | P1:流内早启动独立工具 |
| tool_use/result 配对 | 强制配对;abort/出错都补合成 `tool_result` | 正常路径配对;取消时可能留下无配对 tool_use | **取消路径补合成 tool_result** |
| 取消传播 | AbortController 树 + sibling 级联杀 | 单一 ctx,迭代间+流中段检查 | ctx 已够用;补「优雅排空」 |
| 转向/插话 | `abort('interrupt')` + 排队消息下轮作 attachment 注入 | 无(Stop 即终止) | P1:设计 steer 通道 |
| API 重试 | `withRetry` 指数退避+jitter,按状态码,可切 fallback | 无重试,Stream 出错即失败 | **P0:provider 层加可配置重试(429/5xx/超时)** |
| max_tokens 截断 | 暂扣→升级 64k 重试→多轮「继续」恢复(≤3) | MaxTokens 只是请求参数,截断无感知 | P1:检测 stop_reason=max_tokens,注入「继续」 |
| prompt-too-long | 暂扣→collapse/reactive compact 恢复 | 无 | P2:依赖压缩(见 context-management.md) |
| malformed tool call | zod 校验失败→`InputValidationError` tool_result 喂回自纠 | JSON 解析失败静默丢 ToolInput,仍 dispatch | **P0:校验失败产出 error tool_result,不 dispatch** |
| 未知工具 | 产出 `No such tool` tool_result | 依赖 registry | 对齐:产出 error tool_result |
| 错误是否致命 | 工具错误一律建模为 tool_result,循环继续 | 工具错误进 result.IsError(好);provider/流错误致命 | 工具层保持;流层区分可重试/致命 |
| 事件模型 | async generator yield 消息 | Emitter.Emit 同步推 | 已够用(B/S 扇出);可补 usage/stop_reason 事件 |
| 上下文压缩 | snip/micro/collapse/autoCompact 管线 | 无(历史全量回灌) | 见 context-management.md(独立 capability) |
| 子代理 | 复用 `query` + 隔离 ctx/agentId/sidechain | 无 | P2:复用 Loop + 隔离 Emitter/历史 |
| 可观测性 | usage 累积、queryTracking chainId/depth、每机制 logEvent | `EventMessageStop` 处留了 TODO | P1:记录 usage/stop_reason/每轮 token |

## 差距与行动项

**P0(正确性/健壮性,直接可移植)**
1. **provider 层重试**:`loop.go:104` 的 `Stream` 错误直接致命。加 `withRetry` 式指数退避+jitter,按 HTTP 状态码(408/409/429/5xx、连接错误)决定可重试性,可配置次数;可选 fallback 模型。参照 `withRetry.ts:172,532,698`。
2. **malformed tool call 兜底**:`loop.go:273-278` JSON 解析失败静默丢 `ToolInput` 却仍 dispatch。改为:解析/校验失败就产出 `is_error` 的 `tool_result`(带 `tool_use_id`)喂回模型自纠,**不**进入 dispatch。参照 `toolExecution.ts:617-682`。
3. **取消时补配对 tool_result**:`loop.go:111-117` 取消直接返回,若本轮已 emit 了 `tool_use` 会在历史里留下无配对的 tool_use,下轮请求被 API 拒绝。取消路径应为已发出的 tool call 合成 `tool_result`("cancelled")。参照 `query.ts:125` 与 `:1030-1044`。

**P1(体验/能力)**
4. **max_tokens 截断恢复**:从 `EventMessageStop` 拿 `stop_reason`(目前是空 TODO),若为 `max_tokens` 则注入「继续」meta user 消息重跑(上限几次),而非当轮结束。参照 `query.ts:1203-1267`。
5. **转向/插话(steer)**:B/S 下对应「run 进行中客户端再发一条消息」。机制 = 取消当前轮(保留已 produced)+ 把新 user 消息追加进 history 续跑,而非整个 run 作废。参照 `abort('interrupt')` + 排队注入 `query.ts:1588-1608`。
6. **工具并发分级**:给 `toolruntime` 工具加 `ConcurrencySafe(input) bool`;只读安全工具并发、写工具串行;并发批加上限。参照 `toolOrchestration.ts:91,152`。可进一步做流内早启动。
7. **usage/stop_reason 观测**:`EventMessageStop` 处记录 token usage 与 stop_reason,进事件流,供计费/限流/调试。

**P2(架构级,依赖更大设计)**
8. **上下文压缩/管理**:见 [context-management.md](context-management.md)(独立 capability)。
9. **子代理/fork**:复用 `Loop` + 隔离 Emitter/历史/agentId(sidechain 持久化),参照 `forkedAgent.ts:494,550`。
10. **prompt-too-long 恢复**:依赖压缩能力,暂扣 413→compact→重试。

**不要照抄**
- **AbortController/WeakRef 树**:Go 的 `context.Context` 父子取消已等价,只需补「优雅排空」逻辑。
- **REPL 专属**:ESC 监听、ink 渲染、权限对话框、queued_command 回放。
- **快模式/计费冷却、`x-should-retry` 头、订阅用户判定**:Anthropic 产品策略,多用户平台应实现自己的配额/限流。
- **feature() 门控与 bun:bundle 死代码消除**:构建系统细节,与 Go 无关。
- **大量 logEvent/OTel 遥测**:选取有用的(usage、错误分类、每轮 token)即可。
