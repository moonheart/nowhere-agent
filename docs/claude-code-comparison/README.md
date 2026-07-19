# 功能对比文档说明

本目录对比 **nowhere-agent** 各功能点与 **Claude Code**(`E:\Git_Github\claude-code`,基于 2.1.88 分发 bundle+sourcemap 恢复的 TS 源码,Esonhugh 维护,非官方)的对应实现,作为我们补齐/重构功能时的设计参考。

## 每个文档的结构

- **我们的现状** — nowhere 当前实现(包路径、已完成度、缺口)
- **Claude Code 的做法** — CC 的架构与关键代码(带文件/行号引用)
- **机制对比表** — 逐点对照
- **差距与行动项** — 我们缺什么、该怎么补(标注优先级)

## 重要约束

- CC 是 **TS/CLI 单进程**;nowhere 是 **Go 多用户 B/S 后端**。不能照搬,只做**机制层面的借鉴**(算法、阈值、配对修复、生命周期),不照搬其进程模型/前端。
- CC 大量逻辑挂在 CLI REPL/前端 hooks 上;我们只取其后端可复用的部分。
- CC 引用格式:`src/.../file.ts:行号`。nowhere 引用格式:`internal/.../file.go`。

## 从这里开始

**[00-overview.md](00-overview.md)** — 跨文档综合:两者架构的根本差异、nowhere 领先/落后/独有三大阵营、反复出现的 CC 智慧、聚合成路线图的行动项。**先读这篇。**

## 能力清单(逐项对比)

| 能力 | 文档 | CC 对应子系统 | 一句话结论 |
|---|---|---|---|
| **总览** | [00-overview.md](00-overview.md) | — | 先读 |
| context-management | [context-management.md](context-management.md) | `src/services/compact/` | 🟡 CC 领先:LLM 摘要器+round 切分+配对修复+重注入 |
| agent-loop | [agent-loop.md](agent-loop.md) | `src/query.ts` | 🟡 CC 领先:重试分类+错误建模为 tool_result+优雅排空 |
| provider-abstraction | [provider-abstraction.md](provider-abstraction.md) | `src/services/api/` | 🟢 nowhere 架构更好,补 CC 的工程健壮性 |
| tool-runtime | [tool-runtime.md](tool-runtime.md) | `src/tools/`, `src/Tool.ts` | 🟡 CC 机制多,但不抄本地 fs/shell 实现 |
| sandbox | [sandbox.md](sandbox.md) | sandbox-runtime | 🟡 信任模型相反;学 egress 代理+超时,不抄 seatbelt |
| execution-permission | [execution-permission.md](execution-permission.md) | `useCanUseTool` | 🟡 学规则引擎+异步审批;Checker 须先接入 loop |
| session-runtime | [session-runtime.md](session-runtime.md) | `sessionStorage.ts` | 🟢 **nowhere 数量级领先**(CC 单进程无 attach/run) |
| memory | [memory.md](memory.md) | `src/memdir/` | 🟢 存储领先;学召回层(description+LLM 选择) |
| dreaming | [dreaming.md](dreaming.md) | `autoDream` | 🟢 批式架构更适合多租户;学触发闸门+矛盾检测 |
| skill-system | [skill-system.md](skill-system.md) | `src/skills/` | 🟡 学渐进披露+SkillTool;**先修"看得见用不了"** |
| model-routing | [model-routing.md](model-routing.md) | `src/utils/model/` | 🟢 key 解析领先;学 per-task 分流;自研配额 |
| identity-scope | [identity-scope.md](identity-scope.md) | oauth | 🟢 **CC 空白**,nowhere 自建多租户 |
| workspace-persistence | [workspace-persistence.md](workspace-persistence.md) | fileStateCache | 🟢 持久化领先;学 file-state cache + worktree 模式 |
| observability | [observability.md](observability.md) | analytics + OTel | 🟡 CC 领先:tengu_* 事件 + OTel span 模型 |
