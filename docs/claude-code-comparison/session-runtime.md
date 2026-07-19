# session-runtime vs Claude Code

> 我们的能力:`internal/session`(连接无关 run + EventBus + PG run_events + 多客户端 attach + 对称 cancel)。
> CC 对应:`src/utils/sessionStorage.ts`(JSONL 转录)+ `conversationRecovery.ts` + `branch.ts`(fork)。

## 我们的现状

nowhere 是**服务端、多租户、连接无关**的:

- **Run 是连接无关的实体**(`registry.go:28-87`)。`RunRegistry.Submit` 在专用 goroutine 上执行 run,`runCtx` 刻意**不**派生自调用方 request ctx(D7),提交连接断开后 run 仍存活。
- **Runtime 拥有状态**:single-active-run 锁、run 状态机、`AppendEvent` 单写路径。锁在内存,状态在 PG。
- **EventBus 端口**(`bus.go:10-18`):`Publish`/`Subscribe`,当前 `memBus` 单实例,预留 Redis 跨实例 fan-out(D2)。bus 只是 live-delivery 层,对慢消费者 drop,由 Replay 兜底补 gap。
- **PG run_events 是 source of truth**:`AppendEvent` 先持久化再 fan-out;`EventsAfter(runID, after)` 按 `seq_offset` 有序读回;同时 touch session `updated_at` 供 idle 检测。
- **统一 attach 路径**:`Subscribe`(live)+ `Replay`(offset 补 gap)实现 reconnect-and-replay。
- **对称 cancel**:任意客户端可调 `Cancel`,invoke cancel func 中断 loop+tools。
- **run 生命周期**:`queued/running/waiting_approval/done/failed/cancelled`;registry 保证 terminal 事件在 settle 之前落库+广播(D5,关掉 settle-before-terminal 竞态)。
- **resume 通过 offset replay**,session 有 idle-end 调度。

## Claude Code 的做法

### 1. 消息/转录模型 — 本地 JSONL + parent-uuid 链
CC 的持久化单元是 **JSONL 转录文件**,不是数据库行。
- 每个会话一个文件:`~/.claude/projects/<sanitized-cwd>/<sessionId>.jsonl`(`sessionStorage.ts:204-227`)。子代理(sidechain)写独立文件 `.../subagents/agent-<agentId>.jsonl`。
- 每行一个 Entry,核心转录消息是 `user | assistant | attachment | system` 四类。`progress` 消息**显式排除**在转录与 parent-uuid 链之外(只是临时 UI 状态)。
- **parent-uuid 链**:每条消息带 `parentUuid` 指向上一条链参与者(`sessionStorage.ts:1047-1076`);tool_result 用 `sourceToolAssistantUUID` 指向产生 tool_use 的 assistant 消息。所以转录在磁盘上是一个 **DAG**,不是简单有序日志。
- 写路径是**异步批量 flush**:per-file 写队列 + 100ms 定时 drain,mode 0o600;有 50MB/100MB 读保护阈值(转录可长到数 GB)。
- 读回时**从叶消息沿 parentUuid 反向走回根**重建对话(`buildConversationChain` `:2080-2105`),并用 `recoverOrphanedParallelToolResults` 后处理捞回并行 tool_use 的兄弟分支(`:2129-2217`)。

### 2. 会话持久化与 resume
- **无数据库**,会话就是磁盘上的 JSONL。会话列表靠扫描 project 目录 + 读文件 tail 提取元数据。
- `--continue` 取最近会话(跳过仍在后台写的 live 会话,`conversationRecovery.ts:496-510`);`--resume <id|path>` 按 id/路径加载 → `buildConversationChain` 重建。
- resume 后复用原 sessionId,`adoptResumedSessionFile()` 把指针指向已存在转录继续 append,并恢复大量状态(file-history、todos、worktree、mode)。
- `checkResumeConsistency` 对比 checkpoint 的 messageCount 与重建位置,专抓"写时 397K、resume 读出 1.65M"这类 drift。

### 3. Fork(分支会话)
- `/branch` + `--fork-session`。`createFork`(`branch.ts:61-173`):生成新 sessionId,读当前转录只保留主对话消息;**重新线性化 parent-uuid 链**(把 DAG 压平成单链);每条加 `forkedFrom` 溯源;写入新 `<forkId>.jsonl`。fork 是**离线拷贝文件**操作,产生一个全新独立会话。

### 4. 是否有独立于 session 的 "run"?
**CC 没有 run 这个实体。** 它只区分:
- **一次 agentic turn** = `query()` 生成器(`query.ts:223`)的一次调用,产出直接流给唯一终端 UI。
- **整个 session** = REPL 进程的生命周期,由 JSONL 转录界定。
没有"run 作为可独立寻址、可多客户端观察的对象"。单进程、一次一个 turn、绑定终端。

### 5. 多客户端 / attach
**CC 不支持 attach 到运行中的会话。** 单进程 CLI:一个终端、一个用户、一个 turn 在跑。没有 EventBus、没有订阅者、没有 reconnect-replay。(代码里的 `sessionIngress.ts` 远程 POST、`DIRECT_CONNECT` 是通往 Anthropic 内部 CCR 的钩子,本仓库多为残片,不改变本地单进程本质。)

### 6. 持久日志 vs nowhere 的 run_events + EventBus
CC 的 JSONL ≈ nowhere 的 `run_events`(都 append-only、都是 source of truth),但**没有 live fan-out 层**——CC 产出直接流给内存中的唯一 UI,持久化是旁路异步 flush。nowhere 是"先持久化,再广播给 N 个订阅者,慢者靠 replay 补",CC 完全没有这一层(只有一个消费者)。

### 7. 会话生命周期 / idle
CC 会话**没有显式生命周期状态**,`.jsonl` 文件存在即会话存在;结束 = 进程退出,退出时 cleanup flush。没有 idle-timeout 概念。

## 架构差异(single-process-CLI vs multi-user-server)

这是整篇核心。**CC 和 nowhere 解决的是两个不同的问题,只因都叫 "session" 才放在一起比。**

| 维度 | Claude Code | nowhere |
|---|---|---|
| 进程模型 | 单进程 CLI,一终端一用户 | 多用户服务器,N 并发 |
| run 是否独立实体 | 否,turn 只是 query() 一次调用,绑死终端 | 是,连接无关、可寻址、可多客户端观察 |
| 持久化 | 本地 JSONL,append-only,异步 flush | PG run_events,先持久化 |
| live 分发 | 无(产出给唯一 UI) | EventBus fan-out,慢者 drop+replay 补 |
| attach/重连 | 不支持 | 核心特性:subscribe→replay gap→live-follow |
| resume | 进程重启后整会话重建 | 在线 offset 增量 replay |
| cancel | Esc 中断当前 turn(本地) | 对称:任意客户端 cancel 任意 run |
| 会话生命周期 | 无状态,文件存在即存在 | 显式 active/ended + idle-end 调度 |
| fork | 离线拷贝 JSONL | (无对应物) |

**CC 模型里对 nowhere 真正相关的只有三样:**
1. **parent-uuid 链 / DAG 转录格式**。nowhere 的 `run_events` 是扁平有序日志(`seq_offset`);CC 用 parentUuid 表达 tool_use↔tool_result 的 DAG 与并行分支。若未来要精确重建"哪个 tool_result 对应哪个并行 tool_use"或支持会话内分支,可参考。
2. **fork**。"从某个会话点分叉出新会话"这个产品能力 nowhere 没有,对多用户平台是合理需求。
3. **session 戳元数据**(gitBranch/cwd/version/slug)。CC 每条消息自带运行环境上下文;nowhere 的 run_events 目前只有 kind+payload。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 转录存储 | 本地 JSONL,一会话一文件 | PG run_events 表 | 无(PG 更适合多租户) |
| 消息拓扑 | parent-uuid DAG,读时反向重建 | 扁平 seq_offset 有序日志 | 评估:是否需 parent-uuid 表达 tool 配对/分支 |
| run 实体 | 无(turn=query() 一次调用) | 有,连接无关 | 无(nowhere 更先进) |
| live fan-out | 无 | EventBus,mem→Redis | 无(nowhere 独有) |
| 多客户端 attach | 不支持 | 统一 attach 路径 | 无(nowhere 独有) |
| resume | 重启后整会话重建 | 在线 offset replay | 无(nowhere 更细粒度) |
| cancel | 本地 Esc 中断当前 turn | 对称任意客户端 cancel | 无(nowhere 更通用) |
| fork/分支 | /branch 离线拷贝 JSONL | 无 | 借鉴:作为产品能力加入 |
| 生命周期 | 无状态,文件即会话 | active/ended + idle 调度 | 无(nowhere 更完整) |
| 消息环境戳 | gitBranch/cwd/version/slug | 仅 kind+payload | 借鉴:给 run_events 加环境列 |

## 差距与行动项

诚实结论:**在 session-runtime 这一层,nowhere 比 CC 更先进,且是数量级的差距。** CC 是单进程 CLI,根本没有 nowhere 的核心问题域——没有多客户端、没有 attach、没有连接无关 run、没有跨实例 fan-out、没有显式生命周期。nowhere 的 RunRegistry/EventBus/PG-run_events/对称 cancel/idle-end 这一整套,在 CC 里**一行对应代码都不存在**。

CC 能给 nowhere 的只有格式层面三点小东西:
1. **(低优先级,格式)parent-uuid 链**。若未来需要精确表达并行 tool 配对或会话内 DAG 分支,参考 CC 的 parentUuid + `buildConversationChain` + 孤儿并行 tool_result 恢复。当前扁平 `seq_offset` 对"单一 agent run 顺序回放"已够用,**不急需**。
2. **(产品缺口)fork 会话**。nowhere 可在 PG 上实现:复制某 run 之前的 events 到一个新 session,重写 run seq。比 CC 的离线文件拷贝更干净(有结构化存储)。这是 nowhere **可以新增**的功能。
3. **(低优先级,可观测性)run_events 加环境戳列**。给 run 或 run_events 加 agent 版本/模型/工作区等元数据列,利于审计与跨环境排查。

不需要做的:不要引入 CC 的 JSONL 文件模型、异步 flush 队列、resume-重建(比 nowhere 的在线 replay 粗糙)。这些在服务端多租户语境下都是**倒退**。
