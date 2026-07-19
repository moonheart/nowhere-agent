# observability vs Claude Code

> 我们的能力:`internal/logging`(仅 slog 基础)+ 待办 **15.1–15.4**(tracing/成本计量/结构化日志/replay 视图)。
> CC 对应:`src/services/analytics/`(logEvent + `tengu_*`)+ 全套 OpenTelemetry + `cost-tracker.ts`。

## 我们的现状

nowhere 的可观测性**几乎空白**:

- `internal/logging/logging.go` 只有一个 `New(level, format)`,基于 `log/slog` 输出 JSON/Text 到 stdout。**没有** tracing、metrics、成本计量、按用户/团队的多租户维度、run 级关联 ID、replay。
- 规划中(均未做):**15.1** 分布式 tracing、**15.2** 成本计量、**15.3** 结构化日志、**15.4** run-replay 视图。
- 关键差异:nowhere 是**多租户服务端**,需要自己的、按用户/团队隔离的 tracing/metering/logs;CC 是单用户 CLI,数据要么打终端、要么发往 Anthropic 自家遥测。

## Claude Code 的做法

### 1. Analytics/事件系统(`logEvent` + `tengu_*`)
- **双 sink 架构**:`logEventImpl` 把每个事件**扇出到 Datadog 和第一方(1P)事件日志**两条后端。
- **启动解耦**:sink 未 attach 前事件入内存队列,attach 后用 `queueMicrotask` 异步 drain,不阻塞启动。
- **隐私护栏(类型层)**:用 `never` 标记类型强制开发者显式 cast,**字符串默认禁止**进 metadata,防止误记代码/文件路径。`_PROTO_*` 前缀键是 PII 通道,发往 Datadog 前剥离,只有 1P 导出器能看到。
- **工具名脱敏**:`mcp__<server>__<tool>` 一律改成 `mcp_tool`,内置工具名保留。
- **采样**:按动态配置采样,采样率写入 `sample_rate` 字段。
- **事件命名空间**:全部 `tengu_*` 前缀,**数百个事件**覆盖 api_query/api_error、agent_*、tool_*、cost、compact、permission 全生命周期。
- **统一环境 metadata 富化**:每个事件附 sessionId、userType、subscriptionType、model、entrypoint、platform、version、gitRepoHash、agentContext(agentId/parentSessionId) 等。

### 2. OpenTelemetry(tracing + metrics + logs)
依赖很重(全套 `@opentelemetry/*`)。
- **三信号 Provider**:`initializeTelemetry()` 构建 Meter/Logger/Tracer Provider,service.name=`claude-code`。
- **exporter 由环境变量驱动**:`OTEL_{METRICS,LOGS,TRACES}_EXPORTER` + OTLP protocol(grpc/http-json/http-proto)+ prometheus;需 `CLAUDE_CODE_ENABLE_TELEMETRY=1` 才启用;exporter 按需动态 import。
- **Span 模型(核心可移植点)**(`sessionTracing.ts`):
  - 层级:`interaction`(根,一次用户请求→响应循环)→ `llm_request` / `tool` → `tool.execution` / `hook`。
  - 用 `AsyncLocalStorage` 传递父 span;span 名 `claude_code.interaction` / `claude_code.llm_request` / `claude_code.tool`。
  - LLM span 结束挂 input/output/cache tokens、success、ttft_ms、status_code、error;tool span 挂 tool_name、duration、result_tokens。
- **Metrics counters**:`claude_code.session.count`、`claude_code.lines_of_code.count`、`claude_code.cost.usage`(USD)、`claude_code.token.usage`、`claude_code.code_edit_tool.decision`、`claude_code.active_time.total`。
- **标准 OTel 资源属性(含多租户关键)**:`getTelemetryAttributes()` 给每个 metric/span 附 `user.id`、`session.id`、`organization.id`、`user.account_uuid`、`app.version`。**这正是 nowhere 做 per-user/per-team 计量的维度模型。**
- **BigQuery 导出器**:仅对 API 客户/Enterprise/Team 用户启用,5min 批量导出。

### 3. 成本追踪(`cost-tracker.ts`)
- **价格表** `MODEL_COSTS`:按 canonical model 名映射费率档位,每档含 input/output/cacheWrite/cacheRead/webSearch 单价(per Mtok)。
- **换算** `tokensToUSDCost` =(各 token/1M × rate)之和。
- **未知模型兜底**:用默认模型费率并发 `tengu_unknown_model_cost`,UI 显示"成本可能不准"。
- **累积** `addToTotalSessionCost`:每次 API 响应后累加到 per-model `ModelUsage`,**同时**打到 OTel `costCounter`/`tokenCounter`(按 model + type 属性)。
- **会话持久化**:累计成本/token/时长写入 project config,resume 时按 sessionId 恢复。

### 4. Token/usage 捕获
- 直接读 Anthropic SDK `BetaUsage`(input/output/cache_read/cache_creation/web_search_requests)。
- 每次 LLM 响应 → 同时更新内存计数、OTel counters、LLM span 属性。

### 5. 结构化日志/错误日志(`log.ts`)
- **错误日志 sink 模式**:`ErrorLogSink`,未 attach 前入队,attach 时**立即** drain(错误不能延迟)。
- `logError` 三路:debug 日志文件 + 内存环形 buffer(上限 100 条)+ 持久错误文件。云厂商或 `DISABLE_ERROR_REPORTING` 时直接 return。

### 6. Debug/排障
- **debug 日志文件**:写到 `~/.claude/debug/<session>.log`,维护 `latest` 软链。`--debug` / `DEBUG=1` / `--debug=<pattern>`(带过滤器)/ `/debug` 命令中途开启。
- **最后一次 API 请求捕获**:`captureAPIRequest` 存请求参数(不含 messages,避免内存驻留整个对话),供 bug report。
- **Perfetto tracing**:`CLAUDE_CODE_PERFETTO_TRACE=1` 开启,本地可视化 trace。
- **Transcript replay**:会话以 SerializedMessage JSON 落盘,支持 resume/查看历史。

### 7. 面向用户的成本/状态展示
- **`/cost` 命令**:订阅用户显示订阅状态,API 用户显示总成本、时长、代码行变更、按模型分组的 token 明细。
- **StatusLine**:实时显示成本、input/output tokens、context 窗口用量百分比。
- **CostThresholdDialog**:成本超阈值弹窗。

### CLI 本地 vs 可移植性
- **CLI 本地(不可移植)**:打到终端的 `/cost`、StatusLine、写本地 `~/.claude` 文件、Perfetto、project config 持久化成本。
- **发往 Anthropic 遥测(nowhere 不需要,但模型可借鉴)**:Datadog/1P 事件、BigQuery exporter。
- **可移植到多租户服务端**:`tengu_*` 事件分类法、OTel 三信号 Provider 架构、interaction→llm_request→tool 的 span 层级、`getTelemetryAttributes` 的 user/org/session 维度、token→cost 的费率表与累积逻辑、按 session 聚合 usage。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 事件系统 | logEvent 双 sink 队列,`tengu_*` 命名空间,类型级隐私护栏 | 无 | 借鉴 `tengu_*` 分类法定义 `nowhere_*` 事件;Go 用接口 sink + 队列 |
| Tracing span 模型 | interaction→llm_request→tool→execution/hook,AsyncLocalStorage 传父 | 无 | 用 OTel Go SDK,run→llm→tool span 层级,ctx 传 span |
| Metrics | OTel counters(cost/token/session/loc/active_time) | 无 | 定义 cost/token counter,按 user/team/run 属性 |
| 成本计量 | 费率表 + tokensToUSDCost + 按模型累积 | 无 | 建模型费率表,API 响应后按 usage 累加到 user/team/run |
| Token 捕获 | SDK BetaUsage 4 类 token + websearch | 无 | 在 provider 层捕获 usage,挂 span + 写 metering |
| 多租户维度 | getTelemetryAttributes: user/org/session | 无 | **核心**:resource/span 附 user_id/team_id/run_id |
| 结构化日志 | slog 已有基础;CC 用 debug 文件 + 内存 ring + 错误 sink | 仅 slog JSON stdout | 加 run/request ID 字段,加错误聚合,按租户分文件/标签 |
| Debug/replay | debug 日志文件 + transcript JSON + resume | 无 | run 事件落盘(PG),按 run_id 重建 replay 视图 |
| 导出 | OTLP(grpc/http) + Prometheus + BigQuery | 无 | OTLP exporter + 自有 PG 存储按租户查询 |
| 成本展示 | /cost 命令 + StatusLine + 阈值弹窗 | 无 | Web/API 返回 per-user/per-team 成本 |

## 差距与行动项

两个主要可移植收获:**(a) `tengu_*` 事件分类法**——一套覆盖全生命周期的事件命名 + 统一环境 metadata + 类型级隐私护栏;**(b) OTel span 模型**——interaction→llm_request→tool 的层级 + 上下文传递 + user/org/session 维度属性。nowhere 应把这两者翻译为 Go 多租户版本。

**P0(地基,15.1/15.2 核心)**
1. 引入 OTel Go SDK,建 Meter/Tracer Provider,resource 附 `service.name=nowhere`。
2. 定义 run 级 span 模型:`run`(根,对应 CC interaction)→ `llm_request` → `tool`,用 Go `context.Context` 传 span(替代 CC 的 AsyncLocalStorage)。
3. **多租户维度**:所有 span/metric 附 `user_id`/`team_id`/`run_id`(对应 CC 的 `getTelemetryAttributes`,这是 nowhere 相对 CLI 的关键扩展)。
4. 成本计量:建模型费率表(翻译 `MODEL_COSTS`),在 provider 响应处按 usage 累加到 per-user/per-team/per-run 计数器。

**P1(15.3 + 可查询)**
5. 结构化日志增强:slog 注入 run_id/user_id/team_id 字段;参考 CC 错误 sink 模式做错误聚合。
6. 持久化:run 事件 + usage 落 PG,支持按租户/run 查询(CC 是写本地文件,nowhere 必须服务端存)。
7. OTLP exporter 配置化(对应 CC 的环境变量驱动 exporter)。

**P2(15.4 + UX + 打磨)**
8. run-replay 视图:按 run_id 重建 span 树 + 事件时间线(对应 CC transcript 落盘 + resume)。
9. 成本展示 API:per-user/per-team 成本与 token 明细(对应 `/cost` + StatusLine)。
10. 隐私护栏:事件 metadata 白名单 / 工具名脱敏(对应 CC 的 `sanitizeToolNameForAnalytics` + 类型标记)。
11. 成本阈值告警(对应 CostThresholdDialog)。
