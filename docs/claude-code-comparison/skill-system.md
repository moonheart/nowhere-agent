# skill-system vs Claude Code

> 我们的能力:`internal/skill`(L0/L1/L2 渐进披露 + scope + 版本 D16)。
> CC 对应:`src/skills/` + `src/tools/SkillTool/` + `loadSkillsDir.ts` + `ToolSearchTool`。

## 我们的现状

nowhere 的 skill 系统是**内存态、作用域隔离、带版本**的通用能力包:

- **数据模型**(`skill.go:14-34`):`Skill{ID/Name/Scope/Version/Description(L0)/Body(L1)/Resources(L2)/Scripts(L2)}`,外加 `OverridesVersion`/`NeedsReview` 用于跨作用域 override 审查(D16)。`Scope` 是 `identity.ScopeRef`(system/team/user),天然多用户。
- **Manifest 解析**(`manifest.go`):自研 YAML-lite frontmatter 解析器,只认 `name`/`description`,其余为 markdown body。
- **Store**(`store.go`):内存 `map[id]*Skill` + 锁。`Put` 按 `(name, scope)` 版本自增并标记高优先级 override 需审查;`Get` 按调用方给的 scopes 顺序优先级解析 user>team>system;`List` 返回去重后的 L0 目录。
- **Engine**(`engine.go`):明确的 L0/L1/L2 渐进披露。`LoadL0`/`RenderL0Prompt`、`LoadL1`(body)、`LoadL2Resource`/`LoadL2Script`。
- **ScriptTool**(`scripttool.go`):把 L2 脚本包成 `skill_<skill>_<script>` 工具,在 session sandbox 里 `sh -c` 执行,`RiskSandboxWrite`,60s 超时。
- **注入点**(`chatapi/context.go` + `cmd/server/main.go`):**只有 L0 目录被渲染进 system prompt**(和 memory recall 一起)。

**关键缺口(已确认)**:`main.go` 给 agent loop 传的是**空的** `toolruntime.NewRegistry()`——既没注册任何 ScriptTool,也**没有一个 SkillTool 让模型在运行时拉取 L1 body**。`LoadL1`/`LoadL2*` 原语存在且有测试,但没接进 loop。即:模型只能"看到" skill 名字+一句话描述,**无法主动加载完整指令**,更无法执行脚本。版本/审查(D16)数据结构在,但无 PG 持久化、无回滚、无分发。

## Claude Code 的做法

### 1. Skill 的本质与发现
- Skill 是**文件系统里的目录**:`skill-name/SKILL.md`,frontmatter + markdown body。只支持目录格式。
- 发现路径按优先级:managed(policy)→ user(`~/.claude/skills`)→ project(`.claude/skills`,向上遍历)→ `--add-dir` → legacy `/commands/`(`loadSkillsDir.ts:638-723`)。
- frontmatter 字段丰富:`name/description/when_to_use/allowed-tools/arguments/model/effort/version/disable-model-invocation/user-invocable/hooks/context(fork)/agent/paths/shell`(`:185-265`)。
- 同名去重用 `realpath` 解析符号链接;另有 **conditional skills**(`paths` frontmatter,gitignore 风格匹配,摸到匹配文件才激活)和**动态发现**(读写文件时向上找嵌套 `.claude/skills`)。

### 2. 加载模型(L0/L1/L2 等价物)
- **渐进披露**:turn-0 只把 frontmatter(`name + description + whenToUse`)放进上下文,body 只在调用时加载。
- listing 有**预算**:`SKILL_BUDGET_CONTEXT_PERCENT = 0.01`(上下文窗口的 1%),单条描述硬上限 250 字符;超预算逐步截断,bundled 永不截断(`prompt.ts:23-31, 72-173`)。
- L2 等价物:资源文件不进上下文,body 里写 `Base directory for this skill: <dir>`,模型按需自己 Read/Grep;`${CLAUDE_SKILL_DIR}` 占位符调用时替换。

### 3. 如何呈现给模型
- 通过 **`skill_listing` attachment**(不是 system prompt 主体):`getSkillListingAttachments` 生成。
- **去重发送**:`sentSkillNames`(模块级 Map)记录已发过的 skill,只发增量;`--resume` 时 suppress 避免重复。
- 模型靠 **SkillTool** 的 prompt 决策:"Available skills are listed in system-reminder…this is a BLOCKING REQUIREMENT: invoke the relevant Skill tool BEFORE generating any other response"(`prompt.ts:175-198`)。

### 4. Skill 调用
- SkillTool,input 仅 `{skill, args}`。
- **inline(默认)**:`processPromptSlashCommand` 展开 body(含 `$ARGUMENTS` 替换、inline shell `!…` 执行),作为**新的 user message (`newMessages`)** 注入对话,tool_result 只回 `Launching skill: <name>`。可通过 `contextModifier` 动态改 allowed-tools/model/effort。
- **fork**(`context: fork`):在隔离子 agent 里跑,独立 token 预算,返回结果文本。
- **权限**:`checkPermissions` 按 allow/deny 规则匹配 skill 名,纯声明式 skill 自动放行,否则 ask。

### 5. invoked_skills 跟踪与 compaction 重注入
- 每次调用记录到全局 `STATE.invokedSkills`(key 为 `agentId:skillName`)。
- compaction 时 `createSkillAttachmentIfNeeded`:按 `invokedAt` 倒序,每个 skill 截断到 per-skill 上限,总量受预算限制,打包成 `invoked_skills` attachment 重注入。
- **刻意不重发 skill_listing**(重发 ~4K token 是纯浪费);invoked skill 内容**跨多次 compaction 保留**。

### 6. Skill 搜索/发现
- **ToolSearchTool**:通用 deferred-tool 搜索,`select:<name>` 精确选 / 关键字搜,返回完整 JSONSchema 后工具才可调用。MCP 工具默认 deferred。
- **EXPERIMENTAL_SKILL_SEARCH**(内部实验):远程 skill,从 AKI/GCS 加载注入。
- **热更新**:`skillChangeDetector` 用 chokidar 监听目录,去抖后清缓存 + `resetSentSkillNames` 重公告。

### 7. 版本/分享
- **没有** skill 级版本号语义:`version` frontmatter 只是信息字符串,不参与解析或升级。
- 分发靠 **plugin/marketplace 系统**(git 仓库),review/rollback 不在 skill 层。这与 nowhere 的 `Version`/`NeedsReview`/override 审查(D16)是**不同设计取向**——CC 把版本管理外包给 git/plugin,nowhere 想在 DB 里原生做。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| Skill 载体 | 文件系统 `name/SKILL.md` + 富 frontmatter | DB 友好结构体 + 自研 2-field frontmatter | 保留 DB 模型,frontmatter 字段可借鉴 |
| 发现 | 多目录遍历 + 符号链接去重 + 条件/动态发现 | 内存 Store 按 scope 查询 | 已是 DB 查询;可加 scope 内"条件 skill" |
| L0 呈现 | `skill_listing` attachment,增量去重发送 | `RenderL0Prompt` 进 system prompt,每请求全量 | 改 attachment 式增量注入,省 token |
| L0 预算 | 1% 上下文窗口 + 250 字符/条截断 | 无预算,全量罗列 | 加 token 预算 + 截断 |
| 运行时加载 L1 | **SkillTool**,模型主动调用,body 作为 user message 注入 | **缺失**——`LoadL1` 存在但没接成工具 | **P0:实现 SkillTool 注入 body** |
| L2 资源 | 不进上下文,给 base dir 让模型自取 | `LoadL2Resource` 存在未接线 | 随 L1 body 注入 base dir 提示 |
| L2 脚本执行 | inline shell `!…` 在 prompt 展开时执行 | `ScriptTool` 存在但**未注册进 loop** | **P0:把 ScriptTool 注册进 registry** |
| 调用权限 | allow/deny 规则 + 安全属性自动放行 + ask | 有 `permission.Checker` 按 Risk 判定 | ScriptTool 已带 Risk,接通即可 |
| compaction 重注入 | `invoked_skills` attachment,按预算截断,跨 compaction 保留 | 无(无 invoked 跟踪) | **P1:invoked 跟踪 + compaction 重注入** |
| 增量/热更新 | sentSkillNames 去重 + chokidar 监听重公告 | 无 | P2 |
| 搜索/发现 | ToolSearchTool + 远程 skill 搜索 | 无 | P2 |
| 版本/审查/分发 | 外包给 git plugin/marketplace,skill 无版本语义 | `Version`/`NeedsReview`/override (D16),无持久化/回滚 | **P1:PG 持久化版本 + 审查流**,差异化优势 |

## 差距与行动项

**P0(核心闭环缺失,skill 当前"看得见用不了")**
1. **实现 SkillTool 并注册进 agent loop**:模型按名字调用 → `Engine.LoadL1` 取 body → 作为 user message 注入对话(对标 `SkillTool.ts:638-777`)。这是 CC 渐进披露的核心,也是 nowhere 当前最大的断点——`main.go` 传的是空 registry。
2. **把 ScriptTool 注册进 loop**:L2 脚本执行能力已写好但没接。在 skill 被调用时,把该 skill 的脚本动态注册为可用工具(对标 CC 的 `allowed-tools` + inline shell)。
3. **L1 body 携带 base-dir / 资源索引提示**:让模型知道有哪些 L2 资源可取(对标 `Base directory for this skill:` 头)。

**P1(多轮稳健性 + 我们的差异化)**
4. **invoked_skills 跟踪 + compaction 重注入**:记录本轮调用过哪些 skill,会话压缩后按 token 预算把 body 重注入(对标 `compact.ts:1577-1617`)。nowhere 已有 session replay,需补这条路径。
5. **L0 增量注入 + token 预算**:从"每请求全量渲染进 system prompt"改为 attachment 式增量发送(对标 `sentSkillNames` + `SKILL_BUDGET_CONTEXT_PERCENT`),多轮长会话省 token。
6. **PG 持久化版本 + override 审查流(D16, task 16.7)**:这是 CC **没有**、我们架构天然支持的能力。把内存 Store 换成 PG,`Version`/`NeedsReview` 落库,加回滚。这是 nowhere 多用户场景下相对 CC 文件系统模型的真正优势。

**P2(体验增强)**
7. skill 搜索/发现工具(对标 ToolSearchTool),当 skill 数量上来后避免 L0 目录膨胀。
8. 条件 skill(按 scope/上下文激活,对标 CC 的 `paths` conditional skills,但改成 DB 元数据而非文件路径)。
9. skill 热更新公告(改动后重发 L0 增量)。

**可移植性判断**:CC 的 skill 载体、发现、去重、热更新全是**文件系统/单机的**(`realpath`、chokidar、`.claude/skills` 遍历),不可直接搬到多用户 DB。但其**渐进披露、SkillTool 运行时加载、attachment 增量注入、compaction 重注入、token 预算**这些机制是**纯提示工程/运行时**的,完全可移植到 nowhere 的 DB-backed Store 之上。版本/审查 CC 外包给 git,反而 nowhere 的 DB 原生版本化是更适合多用户的方向。
