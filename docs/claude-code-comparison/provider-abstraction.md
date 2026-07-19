# provider-abstraction vs Claude Code

> 我们的能力:`internal/provider`(provider 中立 block 模型 + Anthropic/OpenAI 适配器)。
> CC 对应:`src/services/api/`(client/claude/withRetry/errors)+ `src/utils/messages.ts`(normalize)+ `src/utils/model/` + `src/utils/context.ts`。

## 我们的现状

nowhere 的 provider 层是**真正的 provider 中立抽象**,设计干净:

- **Canonical block 模型**(`provider/types.go`):自定义 `Block{Type, Text, Thinking, ThinkingSignature, ToolUseID/Name/Input, ToolResultID/Content/IsError, CachePoint}`,四种块 `text/thinking/tool_use/tool_result`;`Message{Role, Content []Block}`;`Request{Model, System, Messages, Tools, MaxTokens, CacheablePrefix}`;`Usage{Input/Output/CacheRead/CacheWrite}`。
- **Adapter 接口**(`adapter.go:43-53`):`Name()` + `Stream(ctx, req) (<-chan Event, error)`,返回事件 channel(有序流,非累积 delta),`EventType` 含 `message_start/block_start/block_delta/block_stop/message_stop/error`。
- **Anthropic adapter**:手写 HTTP + SSE;`buildRequest` 纯函数转换 canonical→API,`CacheablePrefix` 时在 system 块加 `cache_control: ephemeral`;`decodeEvent` 把 `content_block_delta` 的各 delta 映射为统一 `Delta string`。
- **OpenAI adapter**:手写 SSE(`data:`/`[DONE]`);`streamDecoder` 把 OpenAI 累积 chunk 流转成 canonical 事件流,`reasoning_content`→index 0 thinking 块、`content`→index 1 text 块、`tool_calls`→index 2+ tool_use 块。
- **Registry**:名称→adapter 的简单 map。
- **缺口**:无重试/backoff;无 per-model context window / max output 元数据;cache 只在 system 加一个静态断点(无读/写追踪、无 break 检测);token 计数靠粗估(~4 chars/token);无 thinking budget / effort 概念。

## Claude Code 的做法

**总纲:CC 不是 provider 中立抽象。** 它把 **Anthropic Beta SDK 的 `BetaMessageParam`/`BetaContentBlock` 当作唯一 canonical 模型**,让 Bedrock/Vertex/Foundry/OpenAI 全部**伪装成一个 Anthropic client**,在边缘做翻译(OpenAI 最有损)。真正手写/可移植的是:消息历史规范化管线、OpenAI Responses→Anthropic 翻译器、client 工厂路由。

### 1. Provider 抽象 / 消息模型
- Canonical 模型直接 import 自 `@anthropic-ai/sdk`。块类型除 `text/thinking/tool_use/tool_result` 外还有 `redacted_thinking`、`server_tool_use` 等。
- `normalizeMessagesForAPI(messages, tools)`(`messages.ts:2063`):把手富消息清洗成 SDK `MessageParam[]`。关键步骤:剥离 virtual 消息;为之前 400 失败的 PDF/图片建 strip-map;**合并连续 user 消息——注释明说"因为 Bedrock 不支持连续多条 user 消息"**(`:2171-2173`);assistant 侧规整 tool_use、按 `message.id` 合并并发 agent 的同 id 消息;后处理过滤孤立 thinking-only 消息、剥离末尾 thinking 块。
- **Thinking 往返**:signature 随块保留以便 API 校验;不在 thinking 块上放 cache 断点;`filterTrailingThinkingFromLastAssistant` 删末尾 thinking("API 不允许 assistant 以 thinking 结尾");`stripSignatureBlocks` 换凭证后剥掉所有带签名块。

### 2. 多 provider 路由
- `getAPIProvider()`:纯 env 级联 `CLAUDE_CODE_USE_OPENAI>BEDROCK>VERTEX>FOUNDRY>firstParty`。
- `getAnthropicClient()`:每个 provider 返回 SDK `Anthropic` 类型(1P OAuth/apiKey;Bedrock SigV4;Vertex google-auth;Foundry Azure AD;OpenAI 用 createOpenAICompatClient)。

### 3. 流式 / SSE
- 主循环**故意绕过** SDK 的 `BetaMessageStream` helper,直接消费原始 `Stream<BetaRawMessageStreamEvent>`(避免 O(n²) partial JSON 解析)。**事件→消息累积是全手写**。
- `content_block_start` 初始化累积槽;`content_block_delta` 按 delta.type 追加——`input_json_delta` 把工具输入**当字符串累积**;`content_block_stop` 时把累积的 JSON 字符串解析成对象;`message_delta` 折叠最终 usage+stop_reason。
- `updateUsage` 合并 usage,**输入侧 token 加 `>0` 守卫**,防止后续 `message_delta` 的显式 0 覆盖 `message_start` 的真实值。

### 4. 重试 & 错误处理(`withRetry.ts`, `errors.ts`)
- `withRetry` 是 async generator(yield 让 UI 显示倒计时)。**SDK 自带重试被禁用**(主路径 `maxRetries:0`),重试全由 withRetry 负责。
- **退避** `getRetryDelay` = `min(500 * 2^(attempt-1), 32000) + ≤25% jitter`;`Retry-After` 头优先且绕过 maxDelay;默认 10 次,529 上限 3 次。
- **可重试分类 `shouldRetry`**:408/409、连接错误、`overloaded_error`、status≥500 可重试;**429 仅对 PAYG/企业可重试,对订阅者视为 fatal**;401/403-token-revoked 可重试并重建 client。
- **529 → 模型回退**:连续 3 次 529 且有 fallbackModel 则切模型;后台任务 529 直接丢弃防雪崩。
- **流式→非流式回退**:mid-stream 失败默认改用非流式重试。
- **prompt-too-long 两种**:(a) `prompt is too long`(input 超限)→ **触发 context-collapse + 反应式压缩**,`parsePromptTooLongTokenCounts`/`getPromptTooLongTokenGap` 解析 token 缺口决定压缩力度;(b) `input length and max_tokens exceed context limit` → 不压缩,**自动调低 max_tokens 重试**。

### 5. Prompt 缓存
- `getCacheControl()`:恒 `type:'ephemeral'`,可选 `ttl:'1h'`(门控、会话级锁存防 TTL 抖动 bust 缓存)和 `scope:'global'`。
- **断点策略——每请求恰好一个消息级断点**:`addCacheBreakpoints` 把 `cache_control` 放在**最后一条消息**(加在该消息最后一个内容块上,assistant 跳过 thinking 块)。注释解释单断点配合服务端 KV 页逐出。**system 提示**按 `cacheScope` 给静态前缀块加断点;**tools 主循环不加断点**(受 4 断点上限)。
- **缓存读/写追踪**:从流式 usage 读 `cache_read_input_tokens`/`cache_creation_input_tokens`,含 5m/1h 拆分。
- **Cache-break 检测**(`promptCacheBreakDetection.ts`):两阶段。`recordPromptState` 预调用哈希 system/tools/model/betas 存 per-source 状态;`checkResponseForCacheBreak` 在 **`cache_read` 较上次掉 >5% 且绝对值 ≥2000 token** 时判定 break,用预哈希 diff 解释原因(换模型/改 system/改 tools),发事件并写 diff 文件。

### 6. Token 计数 / 用量
- **无 tiktoken**,精确计数走 **Anthropic count_tokens API**:`countMessagesTokensWithAPI`;`countTokensViaHaikuFallback` 走小模型。
- **本地粗估** `roughTokenCountEstimation` = `len/4`(JSON 类 /2),图片/文档固定 2000。
- **压缩决策** `tokenCountWithEstimation` = 上次真实 usage + 之后消息的粗估。

### 7. 模型元数据
- **无单一"模型条目"对象**。context window 是**优先级瀑布** `getContextWindowForModel`:env 覆盖 > `[1m]` 后缀(1M)> 抓取的 capability cache > beta 实验 > 默认 200K。模型族匹配用 canonical 名后 `.includes()` 子串、最具体优先。
- **max output** `getModelMaxOutputTokens` 返回 `{default, upperLimit}` 子串匹配表(opus-4-6=64k/128k、sonnet-4-6=32k/128k…),再加 slot 预留 cap + env 覆盖。
- **能力标志**:thinking、adaptive thinking、effort;数值字段来自 **API 抓取的 capability cache**。
- **Betas/headers**:常量表(interleaved-thinking、context-1m、effort 等),Bedrock 部分 beta 走 body 而非 header。**Thinking 配置**:`{type:'adaptive'}` 或 `{type:'enabled', budget_tokens}`,强制 `budget < max_tokens`。**Effort**:`output_config.effort`,OpenAI 侧映射到 `reasoning.effort`。

**SDK 专属 vs 可移植**:SDK 绑定——块类型、`thinking`/`output_config`/`betas` 请求形状、SSE 事件语法、usage 计量、缓存语义。可移植手写——`normalizeMessagesForAPI` 及其过滤/合并管线、client 工厂路由、`openai-compat.ts` 翻译器。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| Canonical 块模型 | 直接用 Anthropic SDK `BetaContentBlock`(含 redacted_thinking 等) | 自定义 4 种块,含 signature 往返 | **保持我们的**;更干净且跨 provider |
| Provider 抽象策略 | 单一 Anthropic 模型,各家伪装成 Anthropic client,边缘翻译 | 中立 Adapter 接口 + 各 provider 双向翻译 | **保持我们的**;CC 方式绑定 Anthropic SDK |
| 多 provider 路由 | env 级联 + client 工厂 | Registry + LLM_PROVIDER env | 现状够用,无需改 |
| 流式累积 | 手写 event→block 累积,tool input 作字符串累积后解析 | 已有有序事件流 + index 累积 | 对齐:确保 tool_use 的 partial_json 在 block_stop 时解析为 input 对象 |
| 重试/backoff | async generator;exp+jitter(500ms→32s);Retry-After 优先;按错误类型分类 | **无** | **P0**:加 exp+jitter 重试 + 429/529/5xx/连接错误分类 |
| 流式→非流式回退 | mid-stream 失败改非流式重试 | 无 | P1 |
| 模型回退(529) | 连续 3 次 529 切 fallbackModel | 无 | P2 |
| prompt-too-long | 两种:触发压缩 / 自动降 max_tokens 重试 | 仅靠 ~4char/token 预估触发压缩 | **P0**:识别 400 prompt-too-long 触发压缩;P1 解析 token 缺口 |
| 上下文窗口元数据 | 瀑布解析 + capability 抓取 + 子串匹配表 | 无(MaxTokens 由 Config 硬传) | **P0**:建 per-model context window + max output 表 |
| max output tokens | per-model {default,upperLimit} + env 覆盖 | 无 | **P0**(同上) |
| Prompt 缓存断点 | 每请求恰一个消息级断点(最后一条)+ system 静态前缀块;tools 不加 | 仅在 system 加一个静态块 | **P1**:把消息级断点加到最后一条消息末尾 |
| 缓存 TTL/scope | ephemeral + 可选 1h TTL + global scope(门控锁存) | 仅 ephemeral | P2(多用户场景有价值) |
| 缓存读/写追踪 | usage 读 cache_read/creation(含 5m/1h 拆分) | Usage 已含 CacheRead/CacheWrite | 已对齐字段;P2 补 5m/1h 拆分 |
| Cache-break 检测 | cache_read 降>5% 且≥2000 → 哈希 diff 归因 | 无 | P2(观测性,多用户下更有用) |
| Token 计数 | count_tokens API + Haiku 回退 + len/4 粗估 | 仅 len/4 粗估 | **P1**:接 count_tokens API |
| Thinking 配置 | adaptive / enabled+budget,强制 budget<max_tokens | 块可往返但无 budget 概念 | **P1**:请求加 thinking budget 配置 |
| Effort / output_config | output_config.effort + beta header;OpenAI 映射 reasoning.effort | 无 | P2 |
| Betas/headers | 常量表,按 provider 拆分 header/body | 仅 anthropic-version | P1(接 thinking/1M 时按需加) |

## 差距与行动项

**P0 — 正确性/可用性基石**
1. **重试与错误分类**:`Adapter.Stream` 目前一次失败即返回 error。加 exp+jitter(基线 500ms、封顶 32s、单向 ≤25% jitter)、尊重 `Retry-After`、按状态码分类(408/429/5xx/连接错误可重试,4xx 认证/计费不可重试)。参考 `withRetry.ts:532-550` 算法,但**用普通函数+channel 而非 generator**(Go 无对应物)。
2. **模型元数据表**:建 `map[modelFamily]{ContextWindow, MaxOutputTokens{Default,Upper}}`,用 canonical 名 + 子串匹配。当前 MaxTokens 由 Config 硬传、压缩用 100K 假想值(`contextmgmt/compress.go`),极易超窗。
3. **prompt-too-long 检测**:识别 400 `prompt is too long` / `input length and max_tokens exceed context limit`,触发 contextmgmt 压缩(而非当作致命错误)。这是多轮 agent 最常撞的墙。

**P1 — 成本与质量**
4. **消息级缓存断点**:现在只在 system 加静态断点。改成在**最后一条消息的末尾块**加一个断点(单断点策略),让多轮历史命中缓存——这是最大的成本优化。
5. **Token 计数接 API**:Anthropic 适配器加 `count_tokens` 调用,替换 len/4 粗估用于压缩阈值。
6. **Thinking budget 配置**:Request 增加 thinking 配置(enabled+budget / adaptive),并在 Anthropic 请求侧强制 `budget < max_tokens`。目前 thinking 块能往返但没有预算控制。

**P2 — 观测与精细化**
7. **流式→非流式回退**、**缓存读/写 5m/1h 拆分**、**cache-break 检测**(多用户下定位缓存失效很有价值)、**effort/output_config**、**529 模型回退**。

**明确不要抄的(NOT to copy)**
- **不要用 Anthropic SDK 模型当 canonical**。CC 是因绑定 Anthropic 才这么做;nowhere 的中立 block 模型 + 双向适配器更适配多 provider(尤其 OpenAI reasoning_content 已正确映射为 thinking)。CC 的 OpenAI 路径是**有损**的(丢 thinking/signature/缓存),我们的 OpenAI 适配器已比它好。
- **不要抄 normalizeMessagesForAPI 的大部分**:它 90% 是处理 CC 特有的 progress/attachment/virtual/本地命令消息、并发 agent 合并、PDF/图片错误剥离——nowhere 的消息模型简单得多,不需要。唯一值得借鉴的是"**Bedrock 不支持连续多条 user 消息,需合并**"——若未来接 Bedrock 要注意。
- **不要抄 capability 抓取缓存 / GrowthBook 实验 / ant-only 内部模型**:那是 CC 的内部特性开关体系。
- **不要抄 CC 的退避 generator / fast-mode cooldown / 前后台 529 分流**:过度工程,nowhere 用简单函数重试即可。

**一句话总结**:CC 教会 nowhere 的是**围绕 Anthropic API 的工程健壮性**(重试分类、prompt-too-long 恢复、单断点缓存、count_tokens、per-model 元数据、thinking budget),而非架构——nowhere 的 provider 中立抽象本身比 CC 的"Anthropic 中心 + 边缘翻译"更适合多 provider 平台,应保留并补齐上述机制。
