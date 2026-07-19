# workspace-persistence vs Claude Code

> 我们的能力:`internal/workspace`(D4:workspace 即持久数据卷,沙箱是一次性计算)。
> CC 对应:`src/utils/fileStateCache.ts`(内存 file-state)+ `src/utils/fileHistory.ts` + `src/utils/worktree.ts`。**CC 没有 workspace persistence。**

## 我们的现状

nowhere 采用 **D4 设计:workspace 即持久数据卷,沙箱是一次性计算**:

- **抽象 `Store` 接口**(`store.go:34-49`):四原语 `Solidify`(原子持久化 dir 为新版本)、`Materialize`(还原进 dir)、`Current`、`Delete`。注释说明这是 sync(push/pull)模型,远程后端在接口内部翻译成对象存储。
- **内置 `LocalStore`**(`local.go`):布局 `<root>/<sessionID>/{versions/<n>, staging/, current}`。
- **原子 solidify = 三段式两阶段提交**:① 内容写入 `staging/`;② `os.Rename(staging, versions/<next>)`;③ `current` 指针 temp-write + rename。中断在指针更新前,旧版本仍是 current(无部分状态)。
- **Materialize 是 copyDir 还原**;版本号单调递增,`Meta` 记录文件数/字节数。
- **per-session 隔离**靠 `<root>/<sessionID>` 目录分桶。

**未完成**:
- **6.3** Restore on reactivation after long idle(`Store.Materialize` 已有,但"闲置后重激活自动还原"的生命周期钩子未接)。
- **6.4** S3/MinIO 后端 seam(`Store` 接口已留缝,但尚无对象存储实现)。

## Claude Code 的做法

CC 没有 workspace persistence。它直接读写用户的真实文件系统。相关机制只有两类:内存里的 file-state cache(陈旧性检测),以及 git worktree 隔离。

1. **FileStateCache(LRU,内存态)**(`fileStateCache.ts:30-93`):键是 `normalize(path)`,值是 `{content, timestamp, offset, limit, isPartialView}`;上限 100 条 / 25MB。`isPartialView` 标记自动注入造成的部分视图,强制模型先 Read 再写。**活在内存里,不落盘**,唯一序列化是 `cacheToObject()` 喂给 compact。

2. **编辑冲突检测 = timestamp + content 双重比对**(Edit 工具):
   - 文件必须先被读过(`readFileState.get`,否则 errorCode:6),`isPartialView` 同样拦截。
   - `getFileModificationTime > readTimestamp` → 判脏;但 Windows 上 mtime 会无内容变化地抖动(云同步/杀软),所以**全量读时回退做内容比对**消误报。真正脏 → `FILE_UNEXPECTEDLY_MODIFIED_ERROR`。
   - **关键区**:"avoid async operations between here and writing to disk to preserve atomicity"——staleness check 与 write 之间不允许 yield,防并发编辑交错。这是 CC 对"冲突"的全部防护:**单机单进程内的乐观锁**,不是多租户隔离。

3. **Read 去重 stub(`file_unchanged`)**(`FileReadTool.ts:540-573`):同 path+offset+limit 且 mtime 未变,返回 `FILE_UNCHANGED_STUB`("refer to that earlier Read tool_result")省 token。

4. **post-compact 文件还原(内存→上下文,非磁盘)**(`compact.ts:1469-1518`):compact 前快照 readFileState 并 clear,compact 后按 timestamp 取最近 N 个文件、受 token 预算约束,**重新 Read 拿新内容**注入回上下文。这是"恢复模型视野",不是恢复磁盘状态。

5. **`.claude/projects/` 是每项目目录,不是 workspace**:存的是**会话转录** `<sessionId>.jsonl`、subagent 转录、file-history-snapshot 元数据、每项目配置(含 `activeWorktreeSession`)。**没有任何用户源码快照**——源码就在用户磁盘上,无需快照。

6. **file-history(唯一的"持久化 agent 修改"机制)**(`fileHistory.ts`):编辑前备份到 `~/.claude/file-history/<sessionId>/<sha256>@v<n>`;每消息增量备份变更文件;`fileHistoryRewind` 把磁盘回滚到某消息快照。**这是为 undo/checkpoint 服务的单文件粒度备份,写回的还是用户的真实 fs**——不是跨会话的 workspace 持久化,也不是多租户隔离。CAP 100 快照。

7. **git worktree 隔离(任务级,非会话级)**(`worktree.ts`):
   - worktree 落在 `<repoRoot>/.claude/worktrees/<slug>`,分支 `worktree-<slug>`;slug 校验防路径穿越。
   - `getOrCreateWorktree` 快路径复用已存在 worktree(按 HEAD sha 判断);新 worktree 用 `git worktree add`。
   - **持久化指针**:`saveCurrentProjectConfig(...activeWorktreeSession)` 写进项目配置——worktree 跨进程重启可 resume。
   - 子代理隔离:`createAgentWorktree` 不动全局 session 状态;周期清理 `cleanupStaleAgentWorktrees` 扫 ephemeral 命名模式,**fail-closed**(有改动/未推送提交就跳过)。
   - **隔离语义 = 同主机上的独立 git 工作区+独立分支**,靠 git 本身做隔离,不是容器/沙箱。

## 根本差异

**headline:CC 的"workspace"就是用户的真实本地文件系统,持久化由 fs 本身免费给出;nowhere 的 workspace 是必须在多租户沙箱间搬运、跨沙箱重启存活的隔离数据卷。**

| 维度 | Claude Code | nowhere |
|---|---|---|
| 文件落地 | 直接读写用户真实项目目录,**无中间层** | 沙箱内临时目录,结束必须 solidify 出去 |
| 持久化 | 免费——fs 即存储;进程死了文件还在 | 必须主动 `Solidify` 到 `Store`,否则沙箱销毁即丢失 |
| 隔离 | **单租户单主机**;任务级隔离靠 git worktree(同机另一目录) | **多租户**;session 级隔离靠 `<root>/<sessionID>` + 沙箱边界 |
| 快照/还原 | 无 workspace 快照;file-history 只是单文件 undo,写回真实 fs | `Solidify`/`Materialize` 整卷版本化快照与还原(设计核心) |
| 跨会话/重启 | worktree 指针落项目配置;转录落 `.claude/projects/` | 6.3 闲置重激活还原、6.4 S3 后端都还要自建 |
| "冲突"语义 | 单进程内"自我上次读过之后被外部改了"的乐观锁 | 多租户下不同 session/用户的卷一致性、并发 solidify |

CC 之所以**不需要** nowhere 的整套机制:单用户、单主机、agent 就在用户的 cwd 里跑,文件天然持久、天然隔离(就是用户自己的目录)。它要防的只是"我读完之后被 linter/用户改了导致覆盖",所以一个内存 LRU + mtime/content 双检就够。nowhere 是平台:容器是 disposable compute,workspace 必须自己是 durable volume——这是 CC 完全空白、必须自建的部分。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 最近读文件追踪(内容+时间戳) | `fileStateCache` LRU 100/25MB,内存态 | 无 | **借鉴**:Edit/Write 前校验"自读取后被改" |
| 编辑冲突检测 | mtime+content 双检,关键区禁 yield | 无 | **借鉴**:写工具加 staleness guard + 原子 read-modify-write |
| Read 去重 stub | `file_unchanged`/`FILE_UNCHANGED_STUB` | 无 | 可选:省 token,低优先 |
| post-compact 文件还原 | 按 recency+token 预算重读注入 | 无(有 memory recall) | 参考:compact 后恢复"最近文件视野" |
| 每项目/每会话目录 | `.claude/projects/` 存转录+配置,**不含源码** | `<root>/<sessionID>/` 存 workspace 卷 | 本质不同,无需对齐 |
| 持久化 agent 修改 | `fileHistory` 单文件 undo,写回真实 fs | `Store.Solidify` 整卷版本化 | nowhere 已超越 CC |
| workspace 快照/还原 | **不存在** | 原子两阶段提交 + Materialize | nowhere 核心资产,CC 无可借鉴 |
| 闲置重激活还原 | 无(worktree resume 靠 `activeWorktreeSession` 配置指针) | **6.3 未做** | 参考"指针落配置 + 快路径 resume"思路实现 6.3 |
| 任务/会话隔离 | git worktree(fail-closed 清理) | 沙箱边界 + session 目录分桶 | **借鉴**:ephemeral 命名 + fail-closed 周期清理模式 |
| 存储后端 seam | 无(直连本地 fs) | `Store` 接口已留缝,**6.4 S3 未实现** | nowhere 自建,CC 无参照 |

## 差距与行动项

CC 在 workspace persistence 这个 nowhere 的核心命题上**几乎提供不了东西**——它根本没有这个问题。真正可借鉴的只有两块,加上几个小模式:

**P0 — 借鉴 file-state cache + staleness guard(写工具正确性)**
- nowhere 的 Edit/Write 工具目前无"自读取后被改"防护。照搬 CC 三件套:内存 `FileStateCache`(path→{content,timestamp,offset,limit})、写前 `mtime > readTimestamp` 判脏 + 全量读时 content 回退比对消 Windows 误报、staleness-check 与 write 之间禁 yield 的原子关键区。这是 CC 唯一对 nowhere 有普适价值的机制。

**P1 — 借鉴 worktree 的"指针落配置 + fail-closed 周期清理"模式,落地 6.3/6.4**
- 6.3(闲置重激活还原):学 `activeWorktreeSession` 指针写进项目配置、resume 时快路径复用——nowhere 可把 session→workspace `Ref` 指针持久化,重激活时按指针 `Materialize`。
- 6.4(S3 seam):CC 无参照,`Store` 接口已留好缝,直接写 S3 实现即可。
- 清理:学 `cleanupStaleAgentWorktrees` 的 ephemeral 命名模式 + fail-closed(有改动/未提交就跳过),用于 nowhere 回收闲置 session 的旧版本卷。

**P2 — 可选的 token/上下文优化**
- Read 去重 stub(`file_unchanged`)与 post-compact 文件还原(按 recency+token 预算重读)都是 CC 的上下文工程技巧,与 workspace persistence 无关,可后置。

**结论**:headline 差异成立——CC 直连用户真实 fs,无沙箱、无 per-session 隔离、无快照/还原,持久化是 fs 免费给的;nowhere 是多租户平台,必须自建 workspace 隔离 + solidify + restore。CC 唯一值得拿走的是 **file-state cache(陈旧性检测)** 和 **git-worktree 的隔离/清理模式**,其余(整卷版本化、原子 solidify、S3 后端)nowhere 已经走在 CC 前面或 CC 根本不涉及。
