# dreaming vs Claude Code

> 我们的能力:`internal/dreaming`(离线批式记忆固化 worker)+ `internal/scheduler`(周期调度)。
> CC 对应:`src/services/autoDream/`(最接近)+ `src/services/extractMemories/` + `src/services/SessionMemory/`。

## 我们的现状

nowhere 的 "dreaming" 是**离线、批处理、由调度器驱动的服务端记忆固化 worker**:

- **唯一写入者**:Worker 是 long-term memory 的唯一写入方,线上推理路径不直接写长期记忆(`worker.go:2-3`)。
- **数据源是 "episode"**:`EpisodeSource` 接口读取**已结束 session 的持久化 run 记录**(`EndedSessions`/`Episodes`/`MarkProcessed`),按 session 粒度、批式、可标记"已处理"。
- **流水线**:`processSession` 执行 extract → reorganize(compress/reflect 尚未实现)。`extract` 把整段 episode 拼成 prompt 用 LLM 抽取 durable facts;`reorganize` 按 user/team scope 写入 memory,用 `contradicts` 启发式把被否定的旧记忆 `Deprecate`。
- **预算**:`Budget.MaxTokens` 限制单次 dreaming pass 的 LLM token 花费,超预算即停。
- **调度**:`scheduler.Job{Name, Interval, Run}`,1s tick、UTC、启动 catch-up。dreaming 作为其中一个 Job。**尚未接入 cmd/server。**

关键定性:nowhere 的 dreaming 是**多用户服务端架构**——按 scope 隔离、跨 session 批处理、周期 server job、可在空闲时段用便宜模型。

## Claude Code 的做法(最接近的类比)

重要发现:CC **确实有**一个名为 "dream"/"auto-dream" 的记忆固化特性(`src/services/autoDream/`,flag `tengu_onyx_plover`),是 nowhere dreaming 的最直接参照。但 CC 是**单用户、本地 CLI、文件系统记忆**,且所有"后台"工作都**挂在 turn 结束的事件钩子上,而不是独立调度器**。

### 1. extractMemories — turn 级内联记忆抽取
`src/services/extractMemories/extractMemories.ts`
- 在**每个 query loop 结束**(模型给出无工具调用的最终回复)时由 `handleStopHooks` 触发(`stopHooks.ts:149-153`),fire-and-forget。
- 用 `runForkedAgent` 起一个**共享父会话 prompt cache** 的 fork 子代理(`:417-429`),从**当前 session 转录**抽取 durable memory 写入 `~/.claude/projects/<path>/memory/` 的 markdown 文件。
- 游标增量(`lastMemoryMessageUuid`)、主代理已写则跳过(`hasMemoryWritesSince`)、`maxTurns: 5` 硬上限。
- 对应 nowhere 的 **extract** 阶段,但 CC 是**内联、当前 session、turn 级**,不是批式跨 session。

### 2. autoDream — "dream" 记忆固化(最直接参照)
`src/services/autoDream/autoDream.ts`
- 触发点同样是 `handleStopHooks`(`stopHooks.ts:155`)——**仍是 turn 结束钩子,不是定时器**。
- **三道闸门**(`:5-8`):时间闸(距上次 ≥ 24h)→ session 数闸(mtime 新于上次的转录数 ≥ 5)→ 文件锁(`.consolidate-lock`,mtime 即 lastConsolidatedAt,PID 占用 + 1h stale 回收)。
- 通过闸门后用 `runForkedAgent` 起子代理,跑 `buildConsolidationPrompt`——一个 **4 阶段 prompt:Orient → Gather → Consolidate → Prune/Index**,操作 markdown 记忆文件 + 索引 `MEMORY.md`。工具被限制为只读 Bash/Grep/Glob + 仅记忆目录内的 Edit/Write。
- 对应 nowhere 的 **reorganize/prune** 阶段,但 CC 把"抽取哪些事实"交给子代理自主 grep 转录,而非结构化 episode 输入。

### 3. SessionMemory — 内联会话笔记(为 compaction 服务)
`src/services/SessionMemory/sessionMemory.ts`
- 每次采样后内联触发,按阈值(init 10k tokens、每次更新增长 5k tokens、≥3 次工具调用)决定是否 fork 抽取。
- 这是**当前会话的滚动笔记**,喂给 compaction,**不是长期记忆固化**。

### 4. Proactive / 后台任务 / 调度
- `src/proactive/` 是**恢复构建中被阉割**的状态机(空 hook),无法参考。
- `AgentSummary`:每 30s fork 给子代理生成进度短语,**纯 UI 进度展示**,不写记忆。
- CC 唯一的真正调度器是 `cronScheduler.ts` + `ScheduleCronTool`,但那是**用户自己定的 cron 远程代理任务**(最小间隔 1h),与记忆固化无关。
- 其余"后台"是 `backgroundHousekeeping.ts` 的 setTimeout/setInterval 缓存清理,并在启动时 `initAutoDream()`/`initExtractMemories()` 注册钩子。

## 模式差异(inline-summarize vs offline-batch-dreaming)

**CC 没有任何"离线批处理记忆固化 worker"。它的所有记忆动作都是事件钩子驱动的 fork 子代理,挂在 turn 生命周期上。**

| 维度 | Claude Code | nowhere dreaming |
|---|---|---|
| 触发 | **turn 结束钩子**,fire-and-forget | **独立调度器周期 Job**,与 turn 解耦 |
| 数据单元 | **当前 session 转录**(extract)/ mtime 新转录(dream) | **跨已结束 session 的 episode 批**,可 MarkProcessed |
| 执行体 | fork 子代理,**共享主线程 prompt cache**(省钱关键) | 独立 LLM 调用,token 预算封顶 |
| 记忆载体 | 本地 **markdown 文件** + MEMORY.md 索引 | **结构化 memory.Port**,user/team scope 隔离 |
| 并发互斥 | 文件锁 `.consolidate-lock`(单机多进程) | 服务端单 worker,天然互斥 |
| "调度器" | 无(proactive 被阉割;cron 是用户任务) | 真正的 Scheduler(1s tick + catch-up) |

CC 之所以能 fork+cache 内联做,是因为它是**单用户、单进程、本地**:turn 结束顺手起一个共享缓存的子代理,边际成本极低。它的时间闸/数量闸/锁,本质是**"在 turn 钩子里模拟周期性"**——没有独立调度器,就用 mtime 闸门节流。

nowhere 是**多用户服务端**:不能假设有"turn 结束"这个统一事件,也不能为每个用户 fork 一个共享缓存的子代理。所以它选择**批式 worker**:扫一批已结束 session 的 episode、用便宜模型离线固化、按 scope 隔离、token 预算控制成本、调度器在空闲时段跑。**这是对 CC 内联模型的合理发散,而非落后**——CC 的模型在服务端多租户下不可行。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 记忆抽取 (extract) | turn 末 fork,当前 session | 批式 episode→facts | 保留批式;借鉴"主代理已写则跳过"去重 |
| 记忆固化/剪枝 (reorganize/prune) | autoDream 4 阶段子代理 | reorganize + 粗糙否定启发式 | 升级 contradicts 为 LLM 判断;补 compress/reflect |
| 触发方式 | turn 结束钩子 | 调度器周期 Job | 已是目标架构;接入 cmd/server |
| 节流/闸门 | 时间闸 24h + session 数闸 5 + 锁 | token 预算 + 调度 Interval | 可加"距上次 X 小时/累积 N session 才跑"闸门省 token |
| 并发互斥 | 文件锁 + PID | 服务端单 worker 天然互斥 | 无需动作(多副本部署时需分布式锁) |
| 记忆载体 | markdown + MEMORY.md 索引 | memory.Port(结构化,scoped) | 已是目标架构 |
| 成本控制 | fork 共享 prompt cache | Budget.MaxTokens 封顶 | 保留预算;离线时段用便宜模型 |
| 独立调度器 | 无(cron 仅用户任务) | scheduler.go 统一调度 | 已是优势 |

## 差距与行动项

**定性**:nowhere 的 dreaming 在"批式离线固化"这个形态上是 **nowhere-original 的发散**;CC 的真正参照是 **autoDream + extractMemories 这对内联 fork 子代理**。CC 没有调度器驱动的记忆 worker——它用 turn 钩子 + mtime 闸门模拟周期性。

**inline vs batch 权衡(核心论点)**:
- **CC inline(fork+cache)**:优点——无需调度器/锁以外的基建、复用主线程 prompt cache 极省钱、记忆近乎实时。缺点——只在"有 turn"时触发、单用户本地、记忆是松散 markdown、无可控预算、多租户下无法扩展。
- **nowhere batch(调度器+预算)**:优点——天然多租户(scope 隔离)、可离线/空闲跑、可用便宜模型、token 预算可控、episode 可幂等标记重跑。缺点——记忆有延迟(非实时)、需调度器+持久化 episode 基建、当前矛盾检测太弱。

**行动项**:
1. **接入 cmd/server**:dreaming + scheduler 都已实现但未挂载,最高优先级。
2. **借鉴 autoDream 的触发闸门**:在调度 Interval 之外,加"距上次固化 ≥ X 小时 且 累积 ≥ N 个已结束 session 才跑"的闸门,避免空跑烧 token。
3. **升级矛盾检测**:`pipeline.go:32` 的 "no longer" 启发式太弱。参照 CC 把"解决矛盾/删除漂移事实"交给 LLM(`consolidationPrompt.ts`),在 reorganize 阶段做一次 LLM 判矛盾。
4. **补齐 pipeline 的 compress/reflect**:worker.go 注释承诺了 extract→compress→reorganize→reflect 四段,目前只有 extract+reorganize。CC 的 4 阶段 dream prompt(Orient/Gather/Consolidate/Prune)是现成蓝本。
5. **可选 inline 补充**:考虑在 session 结束时同步触发一次轻量 extract(类似 CC 的 turn 末 fork),降低记忆延迟,让批式 dreaming 只做跨 session 的 compress/prune——**内联做"快",批式做"深"**。
6. **多副本部署需分布式锁**:若 server 多副本,dreaming worker 需要等价于 CC `.consolidate-lock` 的分布式锁(DB 行锁/leader election),单副本则无需。
