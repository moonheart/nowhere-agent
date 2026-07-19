# memory vs Claude Code

> 我们的能力:`internal/memory`(读写分离的长期记忆 port + PG FTS/vector 召回)。
> CC 对应:三套并存 — CLAUDE.md 指令文件(`src/utils/claudemd.ts`)+ memdir/auto-memory(`src/memdir/`)+ SessionMemory(`src/services/SessionMemory/`)。

## 我们的现状

- **接口**(`port.go:44-60`):`memory.Port` 读写分离。读侧(在线,agent loop 调用,要求快)只有 `Recall(query, scopes, limit)`;写侧(离线,仅 dreaming worker)有 `Store/Deprecate/Forget/ListByScope`。
- **类型**(`port.go:17-26`):四种 Kind — `fact`/`preference`/`insight`/`summary`。
- **存储模型**:`Memory{ID, Scope(ScopeRef), Kind, Content, Embedding []float32, Deprecated, ...}`,作用域 user/team/system 三级。
- **在线召回**:`PGPort.Recall` 走 Postgres FTS 关键词排序(`pgport.go:55-81`);`RecallVector` 走 pgvector 余弦,**但需调用方自己生成 embedding——agent loop 目前没有 embedder,在线读路径只有关键词**(`pgport.go:86-111`)。
- **离线整合**:独立 "dreaming" worker 把 episode 整合成长期记忆(port 唯一写方),属另一能力。
- **CLAUDE.md / auto-memory**:任务 4.5 已完成——CLAUDE.md 式记忆文件 + auto-memory recall 注入 agent loop。

## Claude Code 的做法

CC 有**三套并存**的记忆机制,全部是**本地文件 + LLM 摘要**,**没有任何向量/语义检索**:

### 1. CLAUDE.md 指令文件(`src/utils/claudemd.ts`)
- **层级加载**,优先级从低到高:Managed(`/etc/claude-code/CLAUDE.md`)→ User(`~/.claude/CLAUDE.md`)→ Project(`CLAUDE.md`、`.claude/CLAUDE.md`、`.claude/rules/*.md`)→ Local(`CLAUDE.local.md`)。`getMemoryFiles` 从 CWD 向上遍历收集,越靠近 CWD 优先级越高。
- 支持 `@path` include 指令(最大深度 5,有循环防护)、frontmatter `paths:` glob 条件规则(访问某文件时才注入匹配规则)。
- 注入:`getClaudeMds` 拼成一段文本,前缀固定指令,作为 system-reminder 注入;大文件截断阈值 40000 字符。嵌套 CLAUDE.md 作为 `nested_memory` attachment 按需注入(用 `readFileState` 去重)。

### 2. memdir / auto-memory(`src/memdir/`)— CC 的持久记忆
- **纯文件系统**:每个 memory 是一个 `.md` 文件,带 frontmatter `{name, description, type}`;目录在 `~/.claude/projects/<git-root>/memory/`,入口索引是 `MEMORY.md`。
- **类型只有四种**:`user/feedback/project/reference`,明确排除可从代码派生的内容。
- **写入**:模型直接用 Write/Edit 工具写文件——先写 topic 文件,再在 `MEMORY.md` 加一行索引。行为指令通过 `loadMemoryPrompt()` 注入 system prompt。
- **召回不是向量,是 LLM 选择**:`findRelevantMemories` 先 `scanMemoryFiles`(按 mtime 排序、封顶 200 个、只读 frontmatter 前 30 行)列出所有 memory 的 `description`,再发一个 sideQuery 给 Sonnet 选最多 5 个相关的(JSON schema 输出)。
- **召回注入**:作为 `relevant_memories` attachment,异步 prefetch 不阻塞主循环,渲染成 system-reminder,带 staleness 提示(>1 天提醒"可能已过期,先验证",`memoryAge.ts:33-42`)。
- **离线提取器 extractMemories**:每个 query loop 结束时 fork 子 agent 把 durable memory 写进 memdir,游标增量、主 agent 已写则跳过(这是 CC 版的 "dreaming",但**会话内联触发**,见 dreaming.md)。

### 3. SessionMemory(`src/services/SessionMemory/`)— 会话内笔记,喂 compaction
- 周期性后台 fork 子 agent,把当前会话笔记维护进**一个 markdown 文件**(固定模板:Session Title/Current State/Errors & Corrections/Worklog 等)。
- 触发阈值:init ≥10k token、每次更新 ≥5k token 增长且 ≥3 次工具调用;`lastSummarizedMessageId` 增量游标;提取 agent 只能 Edit 那一个 memory 文件;每节 ≤2000 token、总 ≤12000。

### 4. AgentSummary / awaySummary — 进度摘要(非持久记忆)
- `AgentSummary`:每 30s fork 生成进度短语,仅供 UI。`useAwaySummary`:终端失焦 5 分钟后生成"你离开期间"摘要。都不持久化。

**关键判断:CC 没有任何 embedding/向量检索。** 所有"语义召回"都靠 **frontmatter `description` + 一个 Sonnet sideQuery 做选择**。存储是**单用户本地文件系统**(按 git-root 隔离项目),team memory 也只是 memdir 下加 `team/` 子目录,靠 sync 共享,没有 DB。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 存储介质 | 本地 `.md` + frontmatter,按 git-root 隔离 | Postgres `memories` 表,scope TEXT owner id | 已领先:DB 天然多用户/可审计 |
| 记忆类型 | 4 类封闭:user/feedback/project/reference | 4 类:fact/preference/insight/summary | 对齐语义;考虑加 reference(外部系统指针)类 |
| 语义召回 | **无向量**。frontmatter description + Sonnet 选 top5 | FTS 关键词(在线);pgvector 余弦已就绪但无 embedder | 给 loop 接 embedder 启用 RecallVector;或借鉴 CC 用 description+LLM 选择省 embedding |
| 召回注入 | relevant_memories attachment → system-reminder,异步 prefetch | 4.5 已注入 auto-memory recall | 补 staleness 提示 + 已 surfacing 去重 |
| 记忆时效 | mtime 算"N days ago",>1 天加"先验证"提醒 | 有 UpdatedAt 但无 staleness 提示 | 召回注入时附"可能过期"提醒 |
| 离线整合 | extractMemories 会话内联 fork,游标增量,主写跳过 | 独立 dreaming worker | 架构不同但等价;借鉴"游标增量+主写跳过"避免重复 |
| 防重复/去重 | 主写与后台提取互斥;alreadySurfaced 过滤 | Deprecated 软删除 | 已有软删除优于 CC;补"召回侧已 surfacing 去重" |
| 指令文件 | CLAUDE.md 层级 + @include + glob 条件规则 | 4.5 已注入 CLAUDE.md 式记忆 | 评估是否需 glob 条件规则 / @include |
| 会话内笔记 | SessionMemory 后台 fork,固定模板,喂 compaction | 无等价机制 | **缺口**:可加会话内滚动摘要喂 compaction |
| 多用户 | 单用户文件系统;team 仅子目录+sync,无真隔离 | scope user/team/system 三级,DB 强制隔离 | 已领先:CC 无对等物,是差异化优势 |

## 差距与行动项

按优先级:

1. **【高 · 召回质量】接通在线向量召回,或采用 description+LLM 选择。** 现状:在线只有 FTS 关键词,`RecallVector` 因 loop 无 embedder 未启用。两条路:(a) 给 loop 配 embedding provider 直接启用 `RecallVector`(DB 式,已就绪);(b) 借鉴 CC——给 `memories` 表加 `description` 短字段,召回时先按 scope 拉候选,再用一个小 LLM 调用做相关性选择(`findRelevantMemories.ts` 的选择 prompt 可直接参考)。(b) 不依赖 embedding 基础设施,多用户下也成立(候选已按 scope 过滤)。

2. **【高 · 注入体验】召回注入加 staleness 提醒 + 已 surfacing 去重。** CC 对 >1 天的 memory 注入"这是 N 天前的快照,先验证再断言",并在选择前过滤已 surfaced 的路径避免重复占用上下文。nowhere 有 `UpdatedAt` 但注入时无时效提示,也无跨轮去重。两者都是纯应用层逻辑,DB 式下用 `UpdatedAt` 即可,多用户安全。

3. **【中 · 能力缺口】会话内滚动摘要(SessionMemory 等价物)。** CC 在后台周期性把会话笔记写进固定模板喂 compaction,带 token/工具调用双阈值和增量游标。nowhere 无等价机制。若需要 compaction 后的连续性,这是真实缺口。DB 式下可存为 `Kind=summary` 的 memory,游标用消息 id。

4. **【中 · 记忆类型】补 `reference`(外部系统指针)类型。** CC 四类型里有专门的 `reference`(Linear/Grafana/Slack 等外部资源指针),nowhere 没有直接对应。可加 `KindReference`,纯枚举扩展。

5. **【低 · 离线整合】借鉴"主写跳过 + 游标增量"避免重复整合。** CC 的 extractMemories 与主 agent 互斥(主写则跳过该区间),游标只处理增量。nowhere 的 dreaming worker 是唯一写方,天然不冲突,但若未来允许 loop 在线写,需要类似去重。

6. **【低 · 指令文件】按需评估 glob 条件规则 / @include。** CC 的 CLAUDE.md 支持 `paths:` glob(访问特定文件才注入对应规则)和 `@path` include。nowhere 4.5 已做基础注入。这些是单机文件系统特性,多用户 DB 式下对应"按 scope/标签条件注入",优先级低。

**总结**:nowhere 在**存储与多用户隔离**上已领先 CC(CC 是单用户本地文件,team 只是子目录+sync,无 DB 强制隔离;nowhere 用 Postgres scope 三级隔离,且有软删除/审计)。CC 值得借鉴的都在**召回体验层**:(1) description+LLM 选择的轻量语义召回(避免 embedding 依赖),(2) staleness 时效提醒,(3) 已 surfacing 去重,(4) 会话内滚动摘要喂 compaction。这些全是应用层逻辑,可直接落在现有 DB 式 port 上,且天然多用户安全。
