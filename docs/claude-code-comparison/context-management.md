# context-management vs Claude Code `services/compact`

> 我们的能力:`internal/contextmgmt`(压缩器)+ 待办任务 **4.4**(把压缩接进 agent loop)。
> CC 对应:`src/services/compact/` 一整个子系统(manual/auto/partial/reactive/micro/snip/session-memory 共 ~7 种变体)。

## 我们的现状

- `internal/contextmgmt/compress.go`:`Policy{MaxTokens, Threshold, KeepRecent}`、`ShouldCompress`、`Compressor interface{ Summarize(dropped) (string, error) }`、`Compress`(滑动窗口:摘要被丢的 → `"[Earlier conversation summarized]\n"+summary` 作 RoleUser 消息,保留最近 KeepRecent 条原文)。
- 12.1–12.3 **已完成**:压缩器作为独立包存在且有测试。
- **4.4 未完成**:压缩器**没人调用**——loop 每次迭代把 `history+produced` 原样塞给 provider,token 只增不减。
- 缺口:① 没接进 loop;② 按消息数切(可能切断 tool_use/tool_result 配对);③ 无 LLM 摘要器实现;④ 无发送前配对修复;⑤ 无反应式 413 兜底;⑥ 无压缩后重注入。

## Claude Code 的做法

compact 是 Claude Code 最成熟的子系统之一。核心机制:

### 1. 摘要器 = 禁用工具的 forked agent,复用主模型与 prompt 缓存
`streamCompactSummary`(`src/services/compact/compact.ts:1181`):
- `canUseTool: deny-all`(`createCompactCanUseTool`, compact.ts:1170)——摘要 agent 只能产文本。
- system prompt: `"You are a helpful AI assistant tasked with summarizing conversations."`;`thinking: disabled`。
- **通过 fork 共享主对话的 prompt 缓存前缀**(`runForkedAgent` + `cacheSafeParams`),实验证实省 ~98% 摘要成本(compact.ts:437-444)。失败回退到普通 streaming。
- 摘要 prompt 是 9 段结构化模板(`src/services/compact/prompt.ts:61` `BASE_COMPACT_PROMPT`):Primary Intent / Key Technical Concepts / Files & Code / Errors & fixes / Problem Solving / **All user messages** / Pending Tasks / Current Work / Optional Next Step。产出 `<analysis>`(草稿,`formatCompactSummary` 用前剥掉, prompt.ts:313)+ `<summary>`。
- `NO_TOOLS_PREAMBLE` + `NO_TOOLS_TRAILER` 双重防工具调用(prompt.ts:19, 269)。

### 2. 工具配对边界 = 按"API round"分组,不是按消息数
`groupMessagesByApiRound`(`src/services/compact/grouping.ts:22`):
- **assistant `message.id` 变化 = 新一轮**。流式 chunk 共享 id,所以边界只在真正新一轮触发。
- API 契约保证"每个 tool_use 在下一 assistant 轮之前必须被 resolve",故按 assistant-id 切分**天然不切断配对**。
- 切/丢都以整组为原子单位(丢最老的组)。

### 3. 双保险:发送前 ensureToolResultPairing 修复
`src/utils/messages.ts:5294`。即使分组切对,resume/截断仍可能有孤儿,每次发 API 前兜底:
- 孤儿 tool_result(无对应 use)→ **剥掉**;剥空塞占位文本(`[Orphaned tool result removed...]`)。
- 悬空 tool_use(无对应 result)→ **合成 `is_error:true` 的 tool_result**(`[Tool use interrupted]`)。
- 重复 tool_use id / 重复 tool_result → 去重。
- 空 assistant content → 塞占位。

### 4. 分层阈值,不是拍脑袋
`src/services/compact/autoCompact.ts`:
- `getEffectiveContextWindowSize = contextWindow − min(maxOutput, 20k)`(autoCompact.ts:38,给摘要预留输出)。
- `getAutoCompactThreshold = effective − 13k`(`AUTOCOMPACT_BUFFER_TOKENS`, autoCompact.ts:67)。
- 再往上:warning(−20k)、error(−20k)、blocking(−3k,到此禁止继续直到压缩)。
- 随 model 的 contextWindow 走;env 可覆盖。
- **熔断器**:连续失败 3 次 → 本 session 不再自动压(`MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES`, autoCompact.ts:75)。

### 5. 压缩后"重注入"与历史的真实去向(精髓,已核实源码)

**先纠正一个容易误解的点:全量压缩不留原文保留尾。**

`compactConversation`(`compact.ts:393`)返回的 `CompactionResult` 是**一组零件**,不含完整新历史;调用方 `query.ts:533` 用 `buildPostCompactMessages`(compact.ts:335)拼成全新数组,然后 `messagesForQuery = postCompactMessages` **整体替换**工作历史。拼装顺序:

```
[ boundaryMarker, ...summaryMessages, ...messagesToKeep?, ...attachments, ...hookResults ]
```

**关键:`messagesToKeep` 只在 `partialCompactConversation`(部分压缩,用户手动 `/compact` 选界,compact.ts:815/1134)里被填充;全量压缩返回里它是 `undefined`。** 所以全量压缩后的工作历史 = **boundary + 摘要 + 重注入附件 + hooks,一条原文消息都不留**(整段对话交给 LLM 摘要器)。"保留最近几条原文"只存在于部分压缩 / reactive / session-memory 那几条 suffix-preserving 路径。

**原始历史:永不删除。** CC 是 append-only JSONL transcript + uuid/parentUuid DAG;压缩产物(新消息)逐条 `yield` **追加**到 transcript 末尾(`query.ts:535`),原始消息仍带原 uuid 留在 DAG 里,供 `--resume`、UI 翻历史。

**resume / 下一轮怎么定位:`getMessagesAfterCompactBoundary`(messages.ts:4795)**——找最后一条 `SystemCompactBoundaryMessage`(压缩时插入,compact.ts:617),只 `slice(boundaryIndex)` 取它之后的消息。boundary 把压缩前的原始消息"挡在门外",API 永远只看到最近一个 boundary 之后的世界。

**重注入什么**(压缩后主动重建上下文,不是丢完留摘要就完事):
- 最近读过的文件 ≤5 个、每文件 ≤5k token、总 50k 预算(`createPostCompactFileAttachments`, compact.ts:1469);已在保留尾的 Read 结果去重。
- 当前 plan、已调用 skill(截断 5k/个、25k 总, `createSkillAttachmentIfNeeded`, compact.ts:1577)、goal、工具 schema 增量、MCP 指令增量。

### 6. 反应式压缩 = 413 兜底
`reactiveCompact` / `truncateHeadForPTLRetry`(compact.ts:247):阈值判断漏了、API 返回 `prompt_too_long` 时,按 round 从头丢最老组(按 tokenGap 或 20%),重试 ≤3(`MAX_PTL_RETRIES`)。最后一道保险防卡死。

### 流程图
```
原始历史 (append-only transcript / 我们的 run_events, 永不删)
  │
  ▼  turn 结束
shouldAutoCompact(tokens ≥ window − 20k输出 − 13k缓冲)
  │  ✗ 熔断: 连失败3次→停用
  ▼
compactConversation()
  1. PreCompact hooks
  2. summary = 禁用工具的 fork agent 读全部历史(共享主缓存)
       └─ 爆窗重试: 按 API-round 丢最老组 ≤3
  3. 新工作历史 = boundary + summary + 重注入附件(文件/plan/skill/goal/工具)
       └─ 全量压缩【不留原文保留尾】;保留尾只在部分压缩
  4. 新历史【整体替换】 messagesForQuery;并【追加】写回 transcript
  5. ensureToolResultPairing 修复(每次发 API 前)
  6. resume/reload → getMessagesAfterCompactBoundary 只取 boundary 之后
  7. PostCompact hooks
```

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 4.4 行动 |
|---|---|---|---|
| 摘要器 | 禁用工具 fork agent,复用主模型+缓存,LLM | `Compressor` 接口,无实现 | **实现 `llmCompressor`**:禁用工具 + 同 model,复用 provider adapter;借鉴 9 段 prompt 精简版 |
| 压缩形态(全量) | 整段历史 → 摘要,**不留原文保留尾** | `Compress` 滑动窗口留 KeepRecent 条原文 | **全量压缩学 CC 不留尾**(整段喂摘要器);"留尾"思路只对齐 CC 的部分压缩 |
| 历史去向 | 原始历史永不删(append-only DAG);工作历史**整体替换**;boundary 标记界限 | run_events 已 append-only;但 loop 每次全文塞 history | **工作视图 vs 持久历史分离**:压缩只改"发给模型的视图",run_events 不动;插 boundary,replay 从边界后重建 |
| 切分边界 | 按 API-round(assistant.id) | 按 KeepRecent 消息数 | **改成按 turn/round 切**:assistant(含 tool_calls)后跟 tool result = 一轮 |
| 配对修复 | ensureToolResultPairing(发送前) | 无 | **必加**:孤儿 result 剥掉、悬空 use 补 error-result。独立于压缩也该有 |
| 阈值 | window − 输出预留 − 缓冲,按 model | `Policy.Threshold 0.8` 比例 | 保留比例;**window 按 model**,预留输出空间 |
| 熔断 | 连失败 3 次停 | 无 | 加上,防压缩反复失败烧 token |
| 压缩后重注入 | 文件/plan/skill/goal/工具 schema | 无 | 4.4 简化:压缩后重注入 **memory recall + skill L0**(复用 4.5 注入机制) |
| 反应式 PTL 兜底 | 413 时按 round 丢头重试 | 无 | **值得做**:provider 报 context-overflow 时丢最老 turn 重试,而非 fail run |
| 启发式兜底 | 仅压缩请求自身爆窗时丢头 | 无 | 启发式只做 PTL 兜底,不做主力 |

## 差距与行动项

**认知更新**:之前推荐"启发式压缩先跑通,LLM 留缝"。看完 CC 改主意——**CC 摘要质量全靠结构化 LLM prompt + 禁用工具的 fork**,启发式只做"压缩请求本身爆窗"的兜底。

**4.4 推荐路线(优先级排序)**:
1. **(P0)工作视图 vs 持久历史分离** — loop 维护两份:持久历史(run_events,append-only,不动)+ 发给模型的"工作视图"。压缩只改工作视图;原始历史永远完整,resume/replay 照旧。CC 用 `messagesForQuery` 替换 + transcript 追加实现了同一件事,我们用"视图"这个概念更干净。
2. **(P0)boundary 标记 + 按 turn/round 切分 + ensureToolResultPairing** — 修 `Compress` 切断 tool 配对的硬伤;配对修复独立成函数,发送前兜底;压缩在工作视图里插边界标记,replay 从边界后重建(接已做的 `serveResume`)。
3. **(P0)LLM 压缩器** — 禁用工具的 sub-call,复用 provider adapter + 精简版结构化摘要 prompt。全量压缩时整段工作视图喂给它,产出摘要替换(不留原文尾)。
4. **(P1)context 预算进 `agent.Config`** — 按 model window,预留输出空间。
5. **(P1)反应式 413 兜底** — 丢最老 turn 重试。
6. **(P1)压缩后重注入 memory/skill**。
7. **(P2)熔断器 + 部分压缩(留尾)** — 部分压缩等用户有"指定从某条消息压缩"诉求时再做。

**不照搬**:CC 的 fork/cache-sharing 是其单进程 CLI 特有,我们用"同 provider adapter 的一次禁用工具调用"即可,不需要 cache-sharing 的复杂度。文件/plan 重注入等 sandbox/tool 落地后再说。
