# nowhere-agent 后端架构审查报告

> **日期**:2026-07-25
> **代码基线**:`master` @ `641a214`(*Archive mcp change; apply specs*)
> **范围**:`internal/` 全部子系统 + `cmd/server` 接线 + `migrations/`。以运行时可达性(从 `cmd/server/main.go` 出发的调用图)为准,而非规格书描述。
> **方法**:通读核心骨架(agent-loop / provider / tool-runtime / session-runtime / registry / handler / subagent / sandbox / schema)+ 三路子系统深挖(持久化正确性、安全姿态、接线完备性)。所有结论均以代码 `file:line` 为据,不复述文档。
> **规模**:生产代码 ~10.5K 行,测试 ~9.3K 行,16 个子系统。

---

## 0. 总体判断

这是一套**内核设计水平相当高、但"平台化"承诺大面积未兑现**的代码库。

一句话概括:**核心 agent 运行时(loop / provider / tool-runtime / session-runtime / subagent)是扎实的、senior 级的端口-适配器设计;但它本质上是一个"穿着多实例外衣的单实例系统",而规格书里的多租户差异化能力(dreaming / 路由 / 技能 / 记忆学习闭环)大多是死代码或未接线。**

加上缺失的韧性与安全控制,当前状态适合做**单实例、可信用户的流式 agent 服务**;离规格书定位的"多租户敌对隔离平台"还有明确距离。三条判断支撑这个结论:

1. **骨架是真的好** —— 端口/适配器贯穿全栈,运行与连接解耦,持久化/实时双轨分离,终态排序严谨。
2. **多实例是半成品** —— Redis broker 暗示多实例,但 EventBus 只有内存实现、单活跃 run 锁在进程内、run 协调与 offset 计数都在进程内。多实例部署会破坏生命周期扇出与单写者不变量。
3. **差异化能力大半不可达** —— dreaming / routing / scheduler 是纯死代码;skill store 从不填充;memory 运行时永不写入。今天它是"单 provider、单实例、带文件工具+子代理+MCP 网搜的流式 chat agent"。

---

## 1. 值得肯定的架构骨架

| 设计 | 好在哪 | 位置 |
|---|---|---|
| **端口-适配器贯穿全栈** | `Store` / `MessageStore` / `StreamBroker` / `EventBus` / `sandbox.Port` / `memory.Port` / `provider.Adapter` 都是干净接口,内存实现与 PG/Redis 实现对称 | 全局 |
| **运行与连接解耦(最强的一块)** | `RunRegistry.Submit` 用 `context.Background()` 派生 run 上下文,run 存活于提交连接之外;提交方与后续 attach 方走**同一条** `attach` 路径,对称消费 | `session/registry.go:92`、`chatapi/handler.go:380` |
| **持久化/实时双轨** | 内容增量走 broker(热路径不落库),装配好的整条消息落 `messages`,生命周期事件落 `run_events`,三者职责清晰;attach 先订阅两路再补历史,内容去重靠 offset 高水位,交接无丢/重窗口 | `session/runtime.go:145`、`chatapi/handler.go:402-429` |
| **终态事件排序严谨** | 先持久化 `KindCancelled`/`done` 再 `CompleteRun`,消除了"attach 方看到 run 已 inactive 但没有终态帧"的竞态 | `session/registry.go:157-165` |
| **provider 中立模型** | 中立 block 模型,thinking + signature 往返、缓存点、图片惰性物化(降级为占位符)、context-overflow 归一为可重试类型错误 | `provider/types.go`、`provider/anthropic/request.go` |
| **subagent 即工具** | 子代理只是另一个 `Tool`,并发/取消/超时全部复用 tool-runtime;深度用 ctx 传递,双重防护(调用级 guard + 满深度剔除 spawn 工具) | `subagent/tool.go`、`subagent/depth.go` |
| **schema 索引到位** | `run_events UNIQUE(run_id,seq_offset)`、`messages UNIQUE(session_id,seq)`、FK 全 `ON DELETE CASCADE`;记忆表 scope 隔离用参数化 SQL,无注入 | `migrations/000002,000006`、`memory/pgport.go:174` |
| **数据面鉴权稳** | 口令 bcrypt(DefaultCost);token 32B `crypto/rand` + SHA-256 落库 + 30 天 TTL;`RequireAuth` 覆盖全部 chat 路由;session/memory 的租户可见性检查到位;无密钥落日志 | `identity/service.go:31,98-110`、`chatapi/handler.go:316`、`memory/pgport.go:174` |
| **测试密度高** | 测试与生产代码近 1:1;pairing/replay/cancel/overflow 等硬约束都有针对性用例 | 全局 |

---

## 2. 关键问题(按主题 + 严重度)

> 严重度:🔴 严重 / 🟠 高 / 🟡 中。编号(A/B/C)在下文与第 4 节路线图、第 5 节索引表间稳定引用。

### A. 韧性 / 正确性(不修会出事故)

**A1 🔴 全代码零 `recover()`**
run worker(`session/registry.go:102`)、每个工具调用(`toolruntime/registry.go:79` 的 `go func`)、SSE 解析(`provider/*/adapter.go` 的 `go streamEvents`)全部是裸 goroutine。**任何一个工具 panic(空指针、越界、类型断言)会直接带崩整个多租户进程**,所有租户的 run 一起死。当前最高优先级的稳定性缺口。

**A2 🔴 provider 层无重试**
`Stream` 收到任何非 200(429/5xx/网络抖动)且非 context-overflow,立即 `KindError` 终止 run(`agent/loop.go:229-238`、`provider/openai/adapter.go:74-82`)。上游网关一次抖动 = 一次 run 失败。

**A3 🔴 崩溃后 run 永久孤儿**
run 上下文源自 `context.Background()`(`session/registry.go:92`),`srv.Shutdown` 既不取消也不 drain;无启动对账、无 reaper。进程重启后 DB 里的 `running` 行永远被 `store.ActiveRun` 当作"活的"返回(`session/runtime.go:213`)→ attach 的 20ms settle-poll 永不终止(`chatapi/handler.go:441`)→ **客户端流永久挂起**。同时 `StartRun` 只查内存 map、不查 DB,新 run 照常开,滞留 `running` 行不断累积。

**A4 🟠 malformed tool-call 静默派发**
`accumulator.finalize` 里 `json.Unmarshal` 失败时 `ToolInput` 留 nil,调用仍照常派发(`agent/loop.go:461-466`),不产出 `is_error` 让模型自纠。派发前也无输入 schema 校验。

**A5 🟠 会话层零事务 + 终态消息静默丢失**
`StartRun`(create + status)、`AppendEvent`(insert event + touch session)都是多条非事务语句(会话层全程无 `BeginTx`,仅 `identity/store.go` 用了);`persistMessage` 忽略 `AppendMessage` 错误(`session/registry.go:263`),最终助手消息一次瞬时 DB 错误就**永久从 `/history` 消失**(broker 副本已清)。

**A6 🟡 崩溃/取消丢失进行中内容**
内容增量只在 broker(易失),持久化边界是装配好的 `KindMessage`。生成中途崩溃/取消,进行中的助手消息从未装配 → 从未持久化,历史里只剩"用户轮 + cancelled 标记",用户刚刚看到的流式文本消失。

### B. 多实例 / 扩展性(与"平台"定位直接冲突)

**B7 🔴 "多实例"是半成品:EventBus 不可换**
`WithBroker` 能把内容广播换成 Redis,但 **`EventBus` 在 `NewRuntime` 里硬编码 `NewMemBus()`,且全仓没有 Redis `EventBus` 实现**(`session/runtime.go:78`)。所以即便 `STREAM_BROKER=redis`,**生命周期事件(running/done/cancelled)也不跨实例**;`bus.go` 里"这是给 Redis 用的端口"的注释没有兑现。跨实例 attach 只能退化到 PG 轮询,`cancelled` 实时帧永远过不去。

**B8 🔴 单活跃 run 锁只在进程内**
`StartRun` 只查内存 `rt.runs`,不查 `store.ActiveRun`(`session/runtime.go:115`);`NextRunSeq`+`CreateRun` 非原子,`UNIQUE(session_id,seq)` 只能防重复 seq、**防不住同会话两个不同 seq 的并发活跃 run**。缺一个 `UNIQUE(session_id) WHERE status IN ('queued','running','waiting_approval')` 的部分唯一索引来做 DB 级单写者兜底。

**B9 🟠 `HTTP_WRITE_TIMEOUT` 默认 60s 会腰斩 SSE**
任何超过 60s 的 agent run,客户端流在 60s 被 `http.Server.WriteTimeout` 掐断(`config.go:105`、`cmd/server/main.go:269`)。因 run 已解耦、可 resume/replay,是**降级 UX(每 60s 需重连)而非丢数据**,但对流式设计是显眼的运维 bug。SSE 场景通常应设 `WriteTimeout=0` 或对流式路由单独放宽。

**B10 🟠 `MessagesFor` 无界全量加载**
每个 chat turn(重建历史)+ 每次 `/history` 都全表拉取整条会话并反序列化每个 JSONB block(`session/pgmessagestore.go:48`、`chatapi/handler.go:217`)。长会话是延迟/内存悬崖 —— 压缩只解决了喂给 LLM 的 token,没解决 DB/Go 侧的加载量。(单查询,无 N+1,但单次即无界。)

**B11 🟠 全局无并发闸**
无 semaphore / errgroup / 限流(grep `golang.org/x/sync`、`Semaphore`、`rate.Limit` 均无)。叠加 subagent 无界扇出(C16)、每事件双写、每客户端 20ms poll,20 连接的池在负载下容易饱和。多个 store 调用跑在 `context.Background()` 上(消息持久化、终态事件),无 statement/query 超时。

**B12 🟡 memBroker 会话 map 慢泄漏**
`Settle` 只把帧环置 nil,从不 `delete(b.streams, sessionID)`(`session/streambroker.go:144`),该 map 按进程生命周期每会话累积一个空 `liveStream`。Redis 实现有 TTL,内存实现没有。

### C. 安全(多租户敌对威胁模型下)

> 数据面隔离(租户 A 拿不到 B 的数据)**成立**;失守的是"敌对 agent 的逃逸面"。

**C13 🔴 出口网络 fail-open 且根本没请求策略**
`NetworkAllowlist` 落到 `bridge`(`sandbox/docker.go:225`),而 `cmd/server/main.go:217` 用零值 `sandbox.Options{}` 调 `Ensure`,`NetworkPolicy` 整套管线从运行服务器**不可达**。租户工具流量可打内网 / 云 metadata / 任意外泄。

**C14 🔴 权限闸是死代码**
`permission.Checker.Check` 只在自己的测试里被调用;派发路径(`agent/loop.go:258` → `toolruntime/registry.go:74`)无任何裁决;`RunWaitingApproval` 定义了却从不被设置(`session/types.go:15`)。所有 `RiskNetwork`(每个 MCP 调用)/`RiskExternalWrite` 工具无条件执行。`Risk` 字段目前纯装饰。

**C15 🔴 Docker 容器零资源限制、零降权**
`HostConfig` 只设了 `NetworkMode`(`sandbox/docker.go:69`)。无 `Memory`/`NanoCPUs`/`PidsLimit`/`CapDrop`/`ReadonlyRootfs`/`User` → 容器内 **root + 全 capability + 可写根 + 无内存/CPU/PID 上限**,单租户可 fork-bomb/OOM 整机。`demuxDockerStream` 还把 exec 输出灌进**无界 `bytes.Buffer`**,`yes` 即 OOM;Exec 无服务端 deadline、无输出看门狗。

**C16 🟠 subagent 扇出无预算**
只有深度上限(默认 3),无总量/宽度/并发/成本闸(grep `subagent/*` 仅见深度 guard)。一个 turn 可发多个 `spawn_agent`,`CallAll` 每个起一个 goroutine,最坏 O(宽^深) 个子 run,各自付费调 LLM。**一个精心构造的 prompt = 数千次嵌套/并行 LLM 调用**(成本放大 + goroutine 耗尽 DoS);5 分钟每子超时不约束聚合扇出。

**C17 🟠 `LocalPort.Exec` 宿主机无约束执行 + skill 参数命令注入**
`exec.CommandContext` 只设 `cmd.Dir`,绕过 `resolve()`(`sandbox/local.go:137`);`skill/scripttool.go:61` 把模型可控的 `args["args"]` 未转义拼进 `sh -c`。**在文档默认的 `SANDBOX_BACKEND=local` 上等于宿主机 RCE**。目前是"上了膛的枪"(`ScriptTool` 未接线,仅测试构造),一旦任何 exec 类工具接到 local 后端即触发。

**C18 🟠 local 写路径 symlink 逃逸**
`resolve()` 的 `EvalSymlinks` **只在目标已存在时**跑(`sandbox/local.go:106`);写新文件时 `os.Lstat` 失败 → 跳过软链检查 → `WriteFile` 直接 `MkdirAll` 穿过软链父目录(`local.go:181`)。配合 C17 可逃出工作区。读路径安全(目标必存在 → EvalSymlinks 必跑)。

**C19 🟡 鉴权与数据落盘的次要缺口**
登录/注册无限流无锁定,且未知邮箱走"快路径"跳过 bcrypt 比较(时序枚举,`identity/service.go:41`);raw LLM 日志把租户 prompt/输出明文落盘,且 `.resp` 用 `os.Create` 是 `0o644` 世界可读(`provider/rawlog.go:49`,`.req` 是 `0o600`);`DB_DSN` 默认 `sslmode=disable` + 内嵌 `postgres:postgres`;无 per-租户 sandbox 数量上限(`sandbox/manager.go`)。**正向**:token 生成/哈希、bcrypt、session 归属检查、memory scope 过滤、ImageStore 路径收敛(`workspace/imagestore.go`,含 EvalSymlinks 复查 + owner 鉴权)均实现正确。

### D. 细节 / 一致性

**D20 🟡 Anthropic 工具缓存注释与代码不符**
`provider/anthropic/request.go:88` 注释"mark the last tool cacheable",但循环从不给任何 tool 设 `CacheCtl`;只有 system block 有缓存点。较大的 tool schema 因此不在缓存前缀内(轻微成本 + 文档漂移)。

**D21 🟡 双重取消系统,一套是死的**
`Runtime.CancelRun` / `RegisterCancel` / `runState.cancel`(`session/runtime.go:261-297`)仅测试触达;生产只走 `RunRegistry.Cancel`。建议删除或统一以减少混淆面。

**D22 🟡 token 用量从不上报**
loop 在 `EventMessageStop` 处只留注释"could be recorded";`handler.go` 的 `finish()` 硬编码 `usage:{inputTokens:0,outputTokens:0}`。无成本计量/可观测,`provider.Usage` 结构形同虚设。

**D23 🟡 无部署/CI 资产**
无 `Dockerfile`、`docker-compose`、`.github/workflows`、`Makefile`;`migrate` 用相对路径 `file://migrations`(须从仓库根运行)。对"多租户平台"定位是明显运维空白。

---

## 3. 能力接线矩阵(规格 vs 运行时真相)

本次审查最重要的发现之一:**规格书里的"平台差异化能力"大半没接到 `cmd/server`。**

| 子系统 | 状态 | 真相 / 证据 |
|---|---|---|
| agent-loop / provider / tool-runtime / **contextmgmt** | ✅ 全接线 | contextmgmt(压缩 `loop.go:396` / 配对 `:200` / 丢最旧轮 `:222` / 持久化截断 `registry.go:127,267`)**全部**在热路径;设计到位 |
| session-runtime / registry / message-store | ✅ 接线 | 单实例下完整;多实例见 B7/B8 |
| subagent(spawn_agent) | ✅ 接线 | 仅内置 `general-purpose`(`agentdef.Builtins()`);磁盘 manifest 从不加载 |
| workspace / ImageStore | 🟡 部分 | 图片存储接线(`main.go:97-100,207`);整卷 solidify/版本快照是孤儿(仅 `local_test.go`) |
| **memory** | 🟡 只读且不学习 | 召回=**关键词全文**(`pgport.go:55`);向量召回 `RecallVector` 孤儿(无 embedder);**写路径只有 dreaming 会写,而 dreaming 从不启动 → 运行时永不写入任何记忆** |
| **skill** | 🟡 看得见用不了 | Engine 接了 prompt(`context.go:48`),但 store 从不被填充(无磁盘加载器)→ L0 永远空;`ScriptTool` 从不注册 → 既不可见也不可执行 |
| **dreaming** | ❌ 纯死代码 | `NewWorker` 仅测试;无任何调度器启动它 |
| **routing**(per-租户 key/模型) | ❌ 纯死代码 | 服务器用 `buildProvider` 的**单一静态 provider**;无 team-key 覆盖、无故障转移 |
| **scheduler** | ❌ 纯死代码 | 全仓无非测试构造点;idle-session 清理因此也没接(且若接上会误杀:纯内容生成从不 bump `updated_at`,`ListIdleSessions` 会把活 run 判为 idle) |

> **含义**:今天它是一个"**单 provider、单实例、带文件工具 + 子代理 + MCP 网搜的流式 chat agent**"。规格书描述的"记忆学习闭环 / 多租户密钥路由 / 后台做梦 / 技能运行时"目前都**不可达**。

---

## 4. 优先级路线图

### P0 — 正确性/安全地基(不做会出 bug 或安全事故)
- **[A1]** 三个 goroutine 派发点(run worker、`CallAll`、stream)加 `recover()` —— 单工具 panic 不能带崩全进程。
- **[A2]** provider 层按状态码分类的重试(指数退避 + jitter);429/529 特殊处理。
- **[A4]** malformed tool-call 转 `is_error`;派发前做输入 schema 校验。
- **[A3]** 启动对账(把滞留 `running` 置 `failed`)+ run reaper;`runCtx` 挂到 shutdown 派生的父上下文并加 run 级超时。
- **[C14]** 把 `Checker.Check` 真正接进派发点;补异步审批状态机(`RunWaitingApproval`)。
- **[C13]** egress 做成 **fail-closed** 并从 config 传真实 `NetworkPolicy`。
- **[C15]** Docker 补 `Resources`(Memory/NanoCPUs/PidsLimit)、`CapDrop:["ALL"]`、非 root `User`、`ReadonlyRootfs`、seccomp;exec 加输出上限 + 超时。
- **[B9]** `HTTP_WRITE_TIMEOUT` 对 SSE 路由置 0(或用 `http.ResponseController` 单独放宽)。

### P1 — 与"多实例/多租户"定位对齐
- **[B7]** 实现 Redis `EventBus` 并 `WithBus` 接线,**或**明确降级为"单实例"并加下一条的部分唯一索引。
- **[B8]** 给 `runs` 加 `UNIQUE(session_id) WHERE status IN (活跃态)` 部分唯一索引,DB 级兜住单活跃 run。
- **[A5]** `StartRun`、`AppendEvent` 包事务;`persistMessage` 停止吞错(至少落日志,理想上反映到终态)。
- **[C16]** subagent 加总量 cap + 并发 semaphore + 跨树共享预算;**[C19]** 补 per-租户 sandbox 配额。
- **[B10]** `MessagesFor` 分页/加界,或缓存重建后的历史。
- **[C17/C18]** 多租户模式下拒绝 `LocalPort.Exec`(exec 只走容器/gVisor);写路径对父链做 symlink 解析或 `O_NOFOLLOW`;skill 参数转义。
- **[C19]** 登录限流/锁定 + 未知邮箱做成常量工作量;raw 日志 `.resp` 收紧到 `0o600` 并在 prod 关闭。

### P2 — 补齐差异化能力(让规格落地)
- **接线 dreaming**(调度器 + 触发闸 + 记忆写回)让记忆真正"学习";给 **skill** store 加磁盘加载器 + 注册 `ScriptTool`(前置:先解决 C17)。
- **接线 routing** 做 per-租户 key 解析 / per-task 模型分流;**[D22]** 补 usage/成本计量(token 上报)。
- **[D21]** 删除/统一死掉的 `Runtime.CancelRun`/`RegisterCancel` 路径;**[D20]** 修 Anthropic 工具缓存;**[D23]** 补 Dockerfile/CI/compose。
- workspace 闲置重激活还原、S3/MinIO 后端、session fork 等产品能力。

---

## 5. 问题索引(便于检索)

| ID | 严重度 | 主题 | 一句话 | 关键位置 |
|---|---|---|---|---|
| A1 | 🔴 | 韧性 | 零 `recover()`,单工具 panic 带崩全进程 | `session/registry.go:102`、`toolruntime/registry.go:79` |
| A2 | 🔴 | 韧性 | provider 层无重试,一次抖动即 run 失败 | `agent/loop.go:229-238` |
| A3 | 🔴 | 韧性 | 崩溃后 run 永久孤儿,attach 永久挂起 | `session/registry.go:92`、`runtime.go:213`、`handler.go:441` |
| A4 | 🟠 | 正确性 | malformed tool-call 静默派发(nil args) | `agent/loop.go:461-466` |
| A5 | 🟠 | 正确性 | 会话层零事务 + 终态消息静默丢失 | `session/registry.go:263`、`pgstore.go:206` |
| A6 | 🟡 | 正确性 | 崩溃/取消丢失进行中内容 | `agent/loop.go:243,270` |
| B7 | 🔴 | 扩展性 | EventBus 硬编码内存,生命周期事件不跨实例 | `session/runtime.go:78` |
| B8 | 🔴 | 扩展性 | 单活跃 run 锁仅进程内,缺 DB 部分唯一索引 | `session/runtime.go:115` |
| B9 | 🟠 | 运维 | `WriteTimeout=60s` 腰斩 SSE | `config.go:105`、`main.go:269` |
| B10 | 🟠 | 扩展性 | `MessagesFor` 每轮无界全量加载 | `session/pgmessagestore.go:48` |
| B11 | 🟠 | 扩展性 | 全局无并发闸/无 query 超时 | 全局 |
| B12 | 🟡 | 扩展性 | memBroker 会话 map 慢泄漏 | `session/streambroker.go:144` |
| C13 | 🔴 | 安全 | egress fail-open 且从不请求策略 | `sandbox/docker.go:225`、`main.go:217` |
| C14 | 🔴 | 安全 | 权限闸是死代码,Risk 纯装饰 | `permission/permission.go`、`agent/loop.go:258` |
| C15 | 🔴 | 安全 | Docker 零资源限制 + root + 无界输出 | `sandbox/docker.go:69,249` |
| C16 | 🟠 | 安全 | subagent 扇出无预算(成本/DoS) | `subagent/tool.go`、`toolruntime/registry.go:74` |
| C17 | 🟠 | 安全 | LocalPort.Exec 宿主机无约束 + 注入 | `sandbox/local.go:137`、`skill/scripttool.go:61` |
| C18 | 🟠 | 安全 | local 写路径 symlink 逃逸 | `sandbox/local.go:106,181` |
| C19 | 🟡 | 安全 | 无爆破防护/枚举时序/日志权限/DSN 明文 | `identity/service.go:41`、`provider/rawlog.go:49`、`config.go:111` |
| D20 | 🟡 | 一致性 | Anthropic 工具缓存注释与代码不符 | `provider/anthropic/request.go:88` |
| D21 | 🟡 | 一致性 | 双重取消系统,一套死代码 | `session/runtime.go:261-297` |
| D22 | 🟡 | 可观测 | token 用量从不上报 | `chatapi/handler.go:586` |
| D23 | 🟡 | 运维 | 无 Dockerfile/CI/compose | 仓库根 |

---

*本报告为 `641a214` 时点快照,随代码演进会过时;引用的 `file:line` 以该基线为准。*
