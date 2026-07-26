# nowhere-agent Agent 能力缺口报告(功能完备性视角)

> **日期**:2026-07-25
> **代码基线**:`master` @ `c95e403`(已含 `fix/arch-review-critical` 合并的 13 项修复)
> **范围**:把 agent 当作"产品"看它**能不能干活** —— 四个功能面:①工具执行面 ②模型/LLM 能力 ③编排/循环/人审 ④记忆与主动性。不重复架构评审已覆盖的韧性/安全问题。
> **方法**:四路并行实地探查(tools / provider / orchestration / memory-skills-proactivity),所有结论以代码 `file:line` 为据,不复述规格。
> **姊妹文档**:`docs/backend-architecture-review-2026-07.md`(韧性/多实例/安全视角)。本文在功能相关处交叉引用其编号(A/B/C/D),并标注哪些已被合并的修复解决。

---

## 0. 总体判断

**基础设施铺得很宽,但真正"接到线、模型当场能用"的能力很窄。**

上一份架构评审看的是"骨架稳不稳、能不能扛多租户";这一份看的是"这个 agent 作为产品,功能面还差什么"。结论一句话:**它今天是一个"单 provider、带三个文件读写工具 + 网页搜索 + 子代理的流式 chat agent",离一个能真正自主干活(尤其是写代码)的 agent 还差两类东西。**

缺口分**两类**,这个区分是本报告最有价值的部分:

- **○ 完全缺失** —— 能力根本没有,需要新建。
- **◐ 已实现但未接线** —— 代码写好了、单测都过了,但 `cmd/server/main.go` 从没实例化/注册,**运行时是死的**。这类占比很大,激活成本低,是性价比最高的活。
- **◑ 半接线 / 能力降级** —— 接了但只覆盖一半,或对某 provider 降级。

> ⭐ 标记 = 高价值、建议优先。

一个反直觉的事实:**当前"缺"的能力里,一大半后端已经写好了,只差最后一根线**。例如执行任意命令 —— `sandbox.Exec` 在 Docker 和 local 后端都完整实现且能用,只是没有任何工具去包它;记忆学习、后台调度、技能运行时同理(全是 ◐)。所以下一步的重心不是"从零造",而是"接线 + 补几个原语工具"。

---

## 1. 现有可调工具面(基线)

模型今天能实际调用的工具,全部由 `cmd/server/main.go:254-282` 的 per-session 绑定器注册,只有三族:

| 工具 | 能力 | 位置 | Risk |
|---|---|---|---|
| `read_file` | 读单个工作区文件(整文件 `io.ReadAll`) | `toolruntime/builtin/files.go:47` | read_only |
| `write_file` | 整文件覆盖(无追加/补丁) | `builtin/files.go:83` | sandbox_write |
| `list_dir` | 列单层目录(非递归) | `builtin/files.go:120` | read_only |
| `mcp_searxng_*` | SearXNG 网页搜索(远程 MCP 代理) | `mcp/tool.go:28` | network |
| `spawn_agent` | 派生一个受限子代理,返回其最终文本 | `subagent/tool.go` | read_only |

**= 3 个文件动词 + 网页搜索 + 子代理。** 下面四节是在此基线之上"还缺什么"。

---

## 2. 缺口清单(按功能面 + 缺口类型)

### T. 工具执行面(最影响"能不能真干活")⭐

> **状态更新**:T1–T5 已在分支 `feat/exec-surface-tools` 落地(`edit_file` / `grep` / `glob` / `run_command`,后者跨平台走统一 POSIX/bash 契约,Windows 用 Git Bash),T2 随 `run_command` 自然可用。**T8 已在分支 `feat/output-pagination-spill` 落地**(`read_file` 加字节 `offset/limit` 分页;`run_command` 超限输出把完整内容溢出到工作区 `.nowhere/tool-results/`、经分页 `read_file` 回取)。T6/T7 仍未做。

**T1 ◐⭐ 无"执行命令(shell)"工具 —— 但后端已实现,只差包一层**
模型跑不了 `git`、编译器、包管理器、任何二进制。`sandbox.Port` 早已定义并实现了 `Exec`:Docker 走 `ContainerExecCreate/Attach`(`sandbox/docker.go:134-167`)、local 走 `exec.CommandContext`(`sandbox/local.go:137-165`)。**唯一的问题是没有任何注册的工具去调它** —— 全树唯一调用方是 `skill.ScriptTool`,而它只在测试里被构造。这是整个报告里 ROI 最高的一项:一个 `run_command` 工具就能把 agent 从"只读"变"能动手"。(接线前须先处理架构评审 **C15**(容器资源限制/降权,已修)与 **C17**(local 后端宿主机 RCE,未修)—— 即 `run_command` 应只允许走加固后的容器后端。)

**T2 ○⭐ 无"运行代码 / 跑测试"能力**
能写代码,但永远无法执行或验证自己写的东西。对一个 coding agent 这是硬伤 —— 没有"写→跑→看报错→改"的闭环。技术上是 T1 的一个应用(经 `Exec` 跑 `go test` / `pytest`),但需要把输出流式回传 + 结果结构化。

**T3 ○⭐ 无精确编辑,只有整文件覆盖**
`write_file` 拿 `path`+完整 `content` 覆盖(`builtin/files.go:83`,底层 `LocalPort.WriteFile` 截断重写 `local.go:181`)。全树无 patch / str-replace / apply-diff(grep `edit_file|apply_patch|str_replace` 无)。改大文件一行要重写全文 —— 烧 token、易误伤、并发下易覆盖。业界 coding agent 的标配 `str_replace` 这里完全没有。

**T4 ○⭐ 无内容搜索(grep)**
找一个符号/字符串得把整文件读进来匹配。无任何 grep / ripgrep / 内容检索工具。这让"在陌生代码库定位实现"极其昂贵(逐文件整读)。

**T5 ○ 无 glob / 递归 find / 目录树**
`list_dir` 只有一层(Docker 跑 `ls -1`,`docker.go:218`)。无模式匹配、无递归发现、无一次性项目树概览。定位文件是 O(逐目录往返),给一个新仓库"建立全局观"需要大量往返。

**T6 ○ 无抓取指定 URL / HTTP 请求 / 浏览器**
网络能力止于 SearXNG **搜索**;没有 fetch 一个具体 URL、读 API、抓文档页、渲染页面的工具。讽刺的是沙箱已有完整的 egress 策略引擎(`NetworkOpen/Allowlist/Deny`,`sandbox/port.go:13-31`,架构评审 C13 已修成 fail-closed)却没有任何工具真正走网络 I/O(除 SearXNG-MCP)。

**T7 ○ 无文件系统变更原语(move/copy/delete/mkdir)**
`Port` 层根本没有 rename/remove/mkdir 动词,也没有工具暴露。`read`+`write`+`list` 三件套删不掉、挪不动文件。做重构/清理类任务直接卡死。

**T8 ✅ 大输出无分页 / 截断即永久丢失 —— 已解决(分支 `feat/output-pagination-spill`)**
~~唯一的大小闸是持久化时 `MaxToolResultChars = 20_000` 硬截断(`contextmgmt/truncate.go:14`),且**剩余部分不落盘、永久丢弃**;工具无 `offset/limit/max_bytes`,`read_file` 整文件 slurp。~~ 现:`read_file` 加字节 `offset/limit` 分页(默认&上限一页 20k,续读标记给出下一 `offset`,`io.LimitReader` 免大文件 OOM);`run_command` 超限输出经 `capAndSpill`(`builtin/spill.go`)把**完整**内容按内容 hash 落盘到 `.nowhere/tool-results/<hash>.txt`,只回 head + 指回 `read_file(path,offset=)` 的标记,分页回取;顺带补齐 docker `WriteFile` 建父目录契约,grep/glob 隐藏该脚手架命名空间。持久化闸退化为**极少触发的兜底**。

> **T 组小结**:这一层是"agent 能真正动手"和"只能读+描述"的分水岭,且 **T1 大半是接线、T3/T4 是补两个标准原语**,投入产出比最高。

---

### L. 模型 / LLM 能力

> **状态更新**:L1、L2 已在分支 `fix/loop-observability` 落地 —— L1 把 stop/finish reason 建进中立 `Event`(两适配器映射),`max_tokens` 截断显式转为错误而非静默完成;L2 累计 token 用量、发 `KindUsage` 终帧、OpenAI 请求带 `include_usage`、finish 帧上报真实 token。L3–L7 仍未做。

> ✅ 已端到端且两适配器齐全:**流式 SSE、工具调用、并行工具调用**(`agent/loop.go` 的 `CallAll` 并发派发多个 tool_use)。以下是缺口。

**L1 ○⭐ stop/finish 原因未建模 —— `max_tokens` 截断对 loop 不可见**
中立 `Event` 没有 stop/finish 字段(`provider/adapter.go:19-44`)。Anthropic 解析出了 `stop_reason` 却从不映射(`anthropic/stream.go:38`),OpenAI 的 `finish_reason` 只当"完成触发器"用、值被丢弃(`openai/stream.go:28,133`)。loop 判定"是否结束"**仅**看 `len(toolCalls)==0`(`agent/loop.go:252`)。后果:模型因 `max_tokens` 被截断时,loop 会当成正常结束 —— 一次静默的错误答复。**接近 bug**。

**L2 ◑⭐ token 用量被丢弃 —— 零成本/用量可观测**(= 架构评审 **D22**)
适配器其实解析了 usage(Anthropic `message_delta`,`anthropic/stream.go:139-146`),但 loop 在 `EventMessageStop` 处**直接扔掉**,只留一句注释 `// usage could be recorded here`(`agent/loop.go:347-348`);`handler` 的 `finish()` 硬编码 `usage:{0,0}`。OpenAI 更是从没在请求里带 `stream_options:{include_usage:true}`(`openai/request.go:14-20`),流式下 usage 永远是 nil。**整个系统没有成本计量、没有 token 可观测**,`provider.Usage` 结构形同虚设。

**L3 ✅ 结构化输出已落地(强制 tool_call 形式)**
中立 `Request` 新增 `JSONResponse *JSONResponseSpec`(`provider/types.go`);两个适配器把合成的"响应工具/函数"追加进 `tools` 并用 `tool_choice` 强制调用(Anthropic `type:"tool"` / OpenAI `type:"function"`),模型必须吐一个符合 schema 的 JSON 对象。实现为**强制 tool_call** 而非各家的 `response_format`/`json_schema` 原生字段 —— 好处是两适配器走同一条已验证的 tool 路径,且答案作为 tool_use 的 JSON input 到达,**结构上把推理/旁白排除在载荷之外**。首个消费方是 dreaming 的 extract/compress/reflect(`ProviderLLM.CompleteJSON` + 类型化 schema)—— 根治了"reasoning 模型把思维链当正文、被逐行解析成 memory"的真 bug(曾在一个会话灌入 77 条、真实账号 1600+ 条垃圾)。**注**:这是"强制工具调用"式的结构化输出,未实现 OpenAI `response_format: json_schema` 原生字段(某些网关对该字段支持更好;需要时可再加一个并行开关)。

**L4 ◑ extended thinking 只能"解析"不能"开启"**
响应侧的思考链路是通的(Anthropic thinking+signature 往返 `anthropic/stream.go:108-137`;OpenAI `reasoning_content`→thinking `openai/stream.go:80-94`),但**请求侧无法启用**:Anthropic 请求体没有 `thinking`/`budget_tokens`(`anthropic/request.go:11-18`),OpenAI 没有 `reasoning_effort`。即:只有 provider 主动吐 reasoning 才看得到,客户端无法主动要更深的思考。

**L5 ◑ 视觉输入仅 Anthropic 可用**
中立模型支持图片(惰性物化),Anthropic 发原生 base64 image source(`anthropic/request.go:125-136`);但 **OpenAI 适配器把图片降级成文本占位符**(`openai/request.go:107-111`,注释"gateway 不接受 image parts")。用 OpenAI 后端时,用户发的图永远到不了模型。

**L6 ◐ 多模型路由 / 降级 / 按请求切模型 未接线**
`routing.Router`(带有序 fallback 列表,`routing/router.go`)存在但**只在测试里被构造**;线上服务器走 `buildProvider` 硬选单一 adapter(`main.go:335-358`),loop 绑死单一 `model`。无客户端选模型、无跨 provider 故障转移、无 per-task 模型分流(子代理可按 def 覆盖 model 是唯一例外)。

**L7 🟡 Anthropic 工具前缀缓存未实现**(= 架构评审 **D20**)
`anthropic/request.go:88` 注释"mark the last tool cacheable",但 `apiTool` 无 `cache_control` 字段、循环也从不设置。较大的 tool schema 因此不在缓存前缀内(轻微成本 + 文档漂移)。

---

### O. 编排 / Agent 循环 / 人审

> **状态更新**:O4 已在分支 `fix/loop-observability` 落地 —— 迭代触顶时补发 `KindError` 终帧(不再静默 failed)。O1/O2/O3/O5/O6 仍未做。

> ✅ 已通:**运行取消全链路**(`chatapi/cancel.go` → `registry.Cancel` → ctx 取消 → 终帧,loop 在 `loop.go:295` 观察);**子代理并行扇出 + 深度/总量/并发三重预算**(架构评审 **C16** 已修,`subagent/tool.go` 的 `WithBudget`);**provider 瞬时错误重试**(架构评审 **A2** 已修,`provider/retry.go`)。以下是缺口。

**O1 ○⭐ 无 planning / TODO 跟踪**
纯 think→tool→think,跨轮唯一状态是原始消息日志 `produced []Message`(`loop.go:193-280`),`Config` 无 plan 字段。没有模型可维护的计划/任务清单工具(业界 agent 的 `update_plan`/`todo` 这里没有),也没有把计划注入 system prompt 或持久化。做多步复杂任务时容易"跑偏"且用户无法看到 agent 的计划。

**O2 ◑⭐ 人在环审批(HITL)是死代码 —— 只有同步"拒绝"闸**
`RunWaitingApproval` 状态**有定义、被 SQL 索引引用,却从不被写入、也无恢复路径**(全树 `UpdateRunStatus` 只在 `runtime.go:150`→running、`:229`→终态)。架构评审 **C14** 我已把同步权限闸接进派发点(deny/allow 生效),但那是"当场拒绝并转 `is_error`",**不是"暂停 run 等人批准再继续"**。服务端把 `"ask"` 直接映射成 deny(`main.go:170-179`),`permission.Approval` 结构只在测试里用。要做"危险工具调用→挂起→推给用户→用户批准/否决→续跑"这套交互,得从头补:审批存储 + 推送 + 裁决回收 + 从 `waiting_approval` 恢复。

**O3 ◑ 无 run 续跑 / 断点恢复**
被打断的 run 无法续跑:启动对账把所有非终态 run 直接标 `failed`(`RecoverStrandedRuns`→`FailStrandedRuns`,架构评审 A3 的取舍)。provider 瞬时**连接**错误现在会重试(A2),但一旦**流中途**断掉或整 run 失败,只能从头开一个新 run —— 没有从最后一条持久化消息续接的能力。

**O4 ○⭐ 迭代上限=静默失败(接近 bug)**
达到 `MaxIterations`(默认 25)时,loop 落出循环返回 `fmt.Errorf("max iterations exceeded")`(`loop.go:279`),**既不发 `KindDone` 也不发 `KindError`**(对比正常结束 `:253`、provider 错误 `:242`)。`registry.execute` 随后把 run 置 `RunFailed`(`registry.go:181`)但只对 panic 发 `KindError` 内容帧。净效果:顶层客户端看到流"无声停止"、run 翻 `failed`,**没有最终答复、没有解释性终帧**。(子代理触顶反而优雅 —— 以 `is_error`+部分输出回给父。)顶层也该同样优雅收尾。

**O5 ◑ `RunQueued` 不是真队列**
行创建时置 `RunQueued`(`memstore.go:98`)后立刻被覆盖成 `RunRunning`(`runtime.go:150`)。并发的第二个 run 被"单活跃"直接 `ErrRunActive` 拒绝(`runtime.go:120-134`),**不排队、不 drain**。"排队执行"这个语义有名无实。

**O6 ○ 子代理严格 fire-and-forget,无协作**
子代理并行扇出、预算受控(已好),但只跑 prompt-only 历史、看不到父对话(`tool.go:102-103,188`),只把最终文本折叠成一条工具结果返回(`collapse.go:19-25`)。**子代理之间无法通信、无共享可变状态、无聚合原语**(map-reduce 式协作)。唯一横向通道是单向 UI 活动 `Sink`,不回流到模型。做"多 agent 分工协作"类任务受限。

---

### K. 记忆 / 技能 / 主动性(差异化能力,大半 ◐ 死代码)

> 本组与架构评审 §3"能力接线矩阵"高度重叠 —— 那里从"接线完备性"记录,这里从"功能后果 + 实现深度"补充。多租户 scope(user/team/system)**已稳固且有测试**(`identity/service.go:79-90`,`memory/pgport.go` scope 过滤),不在缺口内。

**K1 ◐⭐ 记忆写回 / dreaming 已接线(4 阶段,增量)— 对话进行中持续沉淀记忆**
在线路径按设计只读;唯一的写路径 dreaming worker 经 scheduler 周期运行(`DREAMING_ENABLED=true`)。**增量水位模型**:每条会话记 `sessions.dreamed_seq`(messages.id 水位,migration `000009`;取代初版 `000008` 的 ended+布尔标记),worker 只抽取水位**之后**的新消息(`MessagesAfter`),处理后推进水位。因此**活跃会话也能边聊边学、且永远可续聊** —— 学习不再要求会话结束(`ended` 回归"软删除"本义)。生产 `EpisodeSource`(`dreaming/source.go`,PG 走 SQL 自连接 eligibility,内存 store 回退逐会话过滤)、provider 排流的 `LLM`(`llm.go`)均落地并单测。**全 4 阶段已通**:extract→`KindFact`、compress→`KindSummary`(每批小结)、reorganize(矛盾去重)、reflect→`KindInsight`(跨记忆模式 + `deprecate:` 行清理增量重复——正是增量下同一事实反复入库的对策)。reflect/compress 可用 `DREAMING_REFLECT=false` 关掉以省 token(回到 2 阶段 extract→reorganize)。

**K2 ◐⭐ 调度器已启动(驱动 dreaming)— 周期性后台工作打通**
通用 `Scheduler`(命名 interval job、1s tick、启动 catch-up,`scheduler/scheduler.go`)现已在 `main.go` 实例化并 `go Start(ctx)`,随 root ctx 停,以 `DREAMING_INTERVAL` 周期驱动 dreaming worker。**config 开关**:`DREAMING_ENABLED`(默认关,因花钱且需 provider)/`DREAMING_INTERVAL`/`DREAMING_MAX_TOKENS`。附带:调度器"重启 catch-up"仍只靠内存 `lastRun` map,但 dreaming 幂等靠 `dreamed_at`(migration `000008`),重启不会重复处理;沙箱延迟销毁(`Manager.Sweep`)、配额滚动作为后续 job 仍是一行 wrapper 可挂。

**K3 ◐⭐ 技能运行时:只读(K3a)已接线,脚本执行(K3b)仍卡在 C17**
(a) **已 seed**:`skill.LoadDir` 从 `SKILLS_DIR` 把每个含 `SKILL.md` 的子目录 seed 成系统作用域技能(→ `RenderL0Prompt`/`context.go:48` 的 L0 索引点亮,不再恒空);`main.go` 保留并 seed `*skill.Store`。(b) **只读拉取已注册**:`load_skill` 工具(`skill/loadtool.go`,`RiskReadOnly`,调 `Engine.LoadL1`/`LoadL2Resource`)已注册进 tool binder,模型可按需拉技能全文/资源。(c) **脚本执行仍未接**:`ScriptTool`(`sh -c` 拼接未转义模型 args,local 后端 = 宿主机 RCE,即 C17)刻意不注册,disk loader 也跳过 `Scripts` —— 待 C17 修复后再点亮。

**K4 ◐ 向量语义召回缺 embedder,退化为关键词**
在线召回按 Postgres 全文检索 `ts_rank/plainto_tsquery`(`memory/pgport.go:55-81`)。真正的 `RecallVector`(pgvector 余弦,`pgport.go:86-111`)和 `embedding` 列都在,但**全仓没有任何 embedder** 去生成向量(注释 `pgport.go:53-54`:"the agent loop has no embedder")。语义相似召回不可用,只能关键词命中。

---

## 3. 参照系:对照成熟 coding agent 还差什么

仓库自带 `docs/claude-code-comparison/`,以此为标尺,当前明显缺的"标配":

| 成熟 agent 标配 | 本仓状态 | 编号 |
|---|---|---|
| 执行命令 / 跑测试 | ○/◐(后端有 Exec,无工具) | T1,T2 |
| `str_replace` 精确编辑 | ○ 无 | T3 |
| grep / glob 代码检索 | ○ 无 | T4,T5 |
| 抓取 URL / 读网页 | ○ 无(仅搜索) | T6 |
| 计划/TODO 可视化 | ○ 无 | O1 |
| 工具调用人工批准 | ◑ 仅同步拒绝,无挂起-恢复 | O2 |
| token 用量/成本上报 | ◑ 丢弃 | L2 |
| 结构化输出 | ✅ 强制 tool_call 式(见 L3) | L3 |
| 从记忆学习 | ◐ dreaming 已接(4 阶段+增量) | K1 |

> **叙事**:上表左列基本定义了"一个能自主完成开发任务的 agent"。本仓在**执行面**(T)和**学习/主动性**(K)两块差得最多,而这两块又恰好大半是"接线而非造轮子"。

---

## 4. 优先级路线图

### P0 — 让 agent"能真正动手"(执行面,最高价值)
- **[T1]** 新增 `run_command` 工具,包现成的 `sandbox.Exec`;**限定只走加固后的容器后端**(依赖架构评审 C15 已修 / C17 未修)。
- **[T3]** 新增 `edit_file`(str-replace / apply-diff)精确编辑。
- **[T4]** 新增 `grep`/内容检索;**[T5]** 新增 `glob`/递归列举。
- **[T2]** 在 T1 之上做"跑测试"结果结构化回传。

### P1 — 修掉"静默错误"与观测盲区

> **状态**:L1 / O4 / L2 已落地(分支 `fix/loop-observability`);T8 已落地(分支 `feat/output-pagination-spill`)。
- **[L1]** 把 stop/finish reason 建进中立 `Event`,loop 感知 `max_tokens` 截断(别当正常结束)。
- **[O4]** 迭代触顶时优雅收尾:发 `KindError`/合成最终答复 + 终帧,别静默 `failed`。
- **[L2 / D22]** loop 记录 usage、`finish()` 上报真实 token;OpenAI 请求带 `include_usage`。
- **[T8]** 大输出分页 / 截断尾部落盘可回取。

### P2 — 激活"死代码"差异化能力(接线为主)

> **状态**:K2 / K1(4 阶段)/ K3a 已落地(分支 `feat/scheduler-dreaming`)。K3b(脚本执行)仍卡在 C17;K4/L6 未做。
- **[K2]** 启动 `Scheduler`(加 config 开关)→ 作为下面两项的驱动。
  ✅ 已在 `main.go` 实例化并 `go Start(ctx)`(随 root ctx 停),驱动 dreaming job;开关 `DREAMING_ENABLED`(默认关)。调度器 `lastRun` 仍内存态,但 dreaming 幂等靠 `dreamed_at`,故可接受。
- **[K1]** 接线 dreaming(补生产 `EpisodeSource` + `dreamed_at` 列 + compress/reflect 阶段)让记忆真正学习。
  ✅ **4 阶段 + 增量水位已接线**:migration `000009` 把标记从 `dreamed_at`(布尔,`000008`)升级为 `dreamed_seq`(messages.id 水位);`session.Store` 用 `ListUndreamedSessions`/`DreamedSeq`/`MarkDreamedSeq` + `MessageStore.MessagesAfter` 按水位增量抽取;`dreaming.NewStoreSource`/`NewProviderLLM` 为生产实现;scheduler 周期跑 worker 写记忆到 `memory.PGPort`。**活跃会话即可被学习、可续聊**。**compress/reflect 已补**:compress 写 `KindSummary`(每批小结)、reflect 写 `KindInsight` 并经 `deprecate:` 行清理增量重复;`DREAMING_REFLECT=false` 可关(回到 extract→reorganize)。
- **[K3]** 给 skill store 加磁盘/DB 加载器 + 注册技能工具(前置:先解决 C17 的 exec 安全)。
  ✅ **K3a(只读)已接线**:`skill.LoadDir` 从 `SKILLS_DIR` seed 系统作用域技能(跳过脚本)→ L0 索引(`context.go:48`)点亮;只读 `load_skill` 工具(`skill/loadtool.go`,`RiskReadOnly`,不执行)已注册进 tool binder。**K3b(脚本执行 ScriptTool)仍卡在 C17**(模型可控 args 未转义进 `sh -c`,local 后端 = 宿主机 RCE),loader 故意不加载脚本。
- **[K4]** 引入 embedder 点亮向量召回;**[L6]** 接线 routing 做多模型/故障转移。

### P3 — 交互与产品化
- **[O2]** 补异步审批状态机(挂起-推送-裁决-从 `waiting_approval` 恢复)。
- **[O1]** 计划/TODO 工具 + 可视化;**[O3]** run 断点续跑;**[O6]** 子代理协作原语。
- **[L3]** ~~结构化输出~~ ✅ 已落地(强制 tool_call 式,见 §L.L3);**[L4]** 可开启 extended thinking;**[L5]** OpenAI 视觉直通;**[L7/D20]** 工具前缀缓存。

---

## 5. 缺口索引(便于检索)

> 类型:○ 完全缺失 / ◐ 已实现未接线 / ◑ 半接线降级。⭐ = 高价值。

| ID | 类型 | 面 | 一句话 | 关键位置 |
|---|---|---|---|---|
| T1 | ◐⭐ | 工具 | 无 shell 工具,但 `sandbox.Exec` 已实现只差包一层 | `sandbox/docker.go:134`、`local.go:137` |
| T2 | ○⭐ | 工具 | 无运行代码/跑测试,写完无法验证 | (T1 的应用) |
| T3 | ○⭐ | 工具 | 只有整文件覆盖,无精确编辑 | `builtin/files.go:83` |
| T4 | ○⭐ | 工具 | 无内容搜索(grep) | `builtin/files.go` |
| T5 | ○ | 工具 | 无 glob/递归 find/目录树 | `builtin/files.go:120`、`docker.go:218` |
| T6 | ○ | 工具 | 无抓取 URL/HTTP/浏览器(仅搜索) | `mcp/tool.go`、`sandbox/port.go:13` |
| T7 | ○ | 工具 | 无 move/copy/delete/mkdir | `sandbox/port.go` |
| T8 | ✅ | 工具 | read_file 分页 + run_command 溢出落盘可回取 | `builtin/files.go`、`builtin/spill.go` |
| L1 | ○⭐ | 模型 | stop/finish 未建模,截断当正常结束 | `provider/adapter.go:19`、`agent/loop.go:252` |
| L2 | ◑⭐ | 模型 | token 用量被丢弃(=D22) | `agent/loop.go:347` |
| L3 | ✅ | 模型 | 结构化输出已接(强制 tool_call; dreaming 首个消费方) | `provider/types.go`、`provider/{anthropic,openai}/request.go`、`dreaming/llm.go` |
| L4 | ◑ | 模型 | extended thinking 只能解析不能开启 | `anthropic/request.go:11` |
| L5 | ◑ | 模型 | 视觉输入仅 Anthropic,OpenAI 降级 | `openai/request.go:107` |
| L6 | ◐ | 模型 | 多模型路由/降级未接线 | `routing/router.go`、`main.go:335` |
| L7 | 🟡 | 模型 | Anthropic 工具前缀缓存未实现(=D20) | `anthropic/request.go:88` |
| O1 | ○⭐ | 编排 | 无 planning/TODO 跟踪 | `agent/loop.go:193,59` |
| O2 | ◑⭐ | 编排 | HITL 审批死代码,仅同步拒绝 | `session/types.go:15`、`main.go:170` |
| O3 | ◑ | 编排 | 无 run 续跑/断点恢复 | `session/runtime.go:276` |
| O4 | ○⭐ | 编排 | 迭代触顶=静默失败,无终帧 | `agent/loop.go:279` |
| O5 | ◑ | 编排 | `RunQueued` 不是真队列 | `runtime.go:120,150` |
| O6 | ○ | 编排 | 子代理 fire-and-forget,无协作 | `subagent/tool.go:188`、`collapse.go:19` |
| K1 | ◐⭐ | 记忆 | dreaming 已接(4 阶段+增量水位,写 KindFact/KindSummary/KindInsight) | `main.go`(scheduler 驱动)、`dreaming/source.go`、`llm.go`、`worker.go` |
| K2 | ◐⭐ | 主动性 | 调度器已 Start(驱动 dreaming;开关 DREAMING_ENABLED) | `main.go`、`scheduler/scheduler.go` |
| K3 | ◐⭐ | 技能 | K3a 已接(seed + L0 + 只读 load_skill);K3b 脚本执行卡 C17 | `skill/loader.go`、`loadtool.go`、`main.go` |
| K4 | ◐ | 记忆 | 无 embedder,向量召回退化为关键词 | `memory/pgport.go:53,86` |

---

*本报告为 `c95e403` 时点快照,反映功能面而非架构韧性(后者见 `backend-architecture-review-2026-07.md`);引用的 `file:line` 以该基线为准,随代码演进会过时。*
