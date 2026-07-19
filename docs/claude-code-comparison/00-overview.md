# 总览:nowhere-agent vs Claude Code

> 14 个功能点的逐项对比见同目录各文档。本篇做**跨文档综合**:两者架构的根本差异、nowhere 领先/落后的领域、以及聚合成路线图的行动项。

## 一句话根本差异

**Claude Code 是单用户、单进程、本地的 CLI:它把"这台机器上的这一个人"认证到 Anthropic API,在用户自己的文件系统上、自己的终端里跑一个 agent。nowhere-agent 是多用户、多租户的 B/S 服务端平台:它要签发自己的 token、在共享服务器上隔离敌对租户、管理许多并发 run、服务多个客户端。**

这一个差异决定了:**CC 大约一半的子系统(身份、沙箱隔离、工作区持久化、配额、多客户端)在 nowhere 的世界里根本不存在或威胁模型相反;而 nowhere 的核心资产(session-runtime、identity、多租户配额)在 CC 里一行对应代码都没有。**

## 三大阵营

### 🟢 nowhere 已经领先(CC 没有或更弱)
| 能力 | 为什么领先 |
|---|---|
| **session-runtime** | CC 是单进程 CLI,没有连接无关 run、没有多客户端 attach、没有 EventBus fan-out、没有对称 cancel、没有显式生命周期。nowhere 的 RunRegistry/EventBus/PG run_events 这一整套 CC 里不存在,**数量级领先**。 |
| **identity-scope** | CC 无多用户概念(只有"本机这一人"→API 的 OAuth)。nowhere 的三级 scope + 团队 + 自签 token 是 CC 空白。 |
| **provider-abstraction** | CC 绑定 Anthropic SDK,各家伪装成 Anthropic client(OpenAI 路径有损)。nowhere 的中立 block 模型 + 双向适配器更适合多 provider,OpenAI reasoning 映射已比 CC 好。 |
| **dreaming** | CC 只有 turn 末内联 fork(模拟周期性),无独立调度器。nowhere 的批式 worker + scheduler + scope 隔离 + token 预算是更适合多租户的架构。 |
| **memory 存储层** | CC 是单用户本地 markdown 文件 + LLM 选择,team 只是子目录+sync。nowhere 的 PG scope 三级隔离 + 软删除/审计已领先。 |
| **model-routing 的 key 解析** | CC 单 key 单用户。nowhere 的 team-key-override-else-platform 多租户解析已领先。 |
| **workspace 持久化** | CC 直连用户真实 fs,无快照/还原(持久化是 fs 免费给的)。nowhere 的原子 solidify + 整卷版本化是 CC 空白。 |

### 🟡 各有所长 / 模式不同(借鉴机制,不照搬架构)
| 能力 | 关键差异 | 该学什么 |
|---|---|---|
| **context-management** | CC 的 compact 是一整个子系统(7 种变体);nowhere 只有孤儿压缩器 | 学:LLM 摘要器、按 API-round 切分、ensureToolResultPairing、分层阈值、压缩后重注入、反应式 413 兜底、熔断 |
| **agent-loop** | CC 是 async-generator 状态机,多层错误恢复;nowhere 是固定 25 次迭代,错误即死 | 学:重试分类、malformed tool call 兜底、取消时补配对 tool_result、max_tokens 恢复、steering |
| **memory 召回层** | CC 召回用 description+LLM 选择(不依赖 embedding) | 学:轻量语义召回、staleness 提醒、已 surfacing 去重、会话内滚动摘要 |
| **dreaming** | CC 内联 fork 共享缓存(省钱、实时);nowhere 批式(可控、多租户) | 学:触发闸门(时间+数量)、LLM 矛盾检测、4 阶段 prompt;可选内联做"快"、批式做"深" |
| **tool-runtime** | CC 契约超宽(执行+权限+schema+UI);nowhere 窄而干净 | 学:输入校验、结果持久化预览、并发安全分级、MCP seam;不抄 UI 渲染 |
| **sandbox** | CC seatbelt/bwrap(同内核)< nowhere Docker;但 CC 有 egress 代理 | 学:egress 代理、Exec 超时、输出看门狗;不抄 seatbelt/交互审批/fallback 非沙箱 |
| **execution-permission** | CC 规则引擎+模式+危险命令启发式;nowhere 只有 Risk 静态映射 | 学:把 Checker 接入 loop、异步审批生命周期、审计落库、建议规则 |
| **model-routing** | CC 有 per-task 小模型分流 + 成本追踪;nowhere 无模型选择/计量 | 学:per-task 分流、Meter 成本归属;自研两级配额(CC 无) |
| **observability** | CC 有完整 OTel + tengu_* 事件;nowhere 仅 slog | 学:tengu_* 事件分类法、interaction→llm→tool span 模型、user/org/session 维度、token→cost 费率表 |
| **workspace-persistence** | CC 有 file-state cache + worktree;nowhere 有整卷版本化 | 学:file-state cache 陈旧性检测、worktree 隔离/清理模式 |

### 🔴 nowhere 独有(CC 完全没有,无参照,自研)
- **多租户配额与限流**(task 14.3)——CC 把配额推给 Anthropic 服务端。
- **多客户端 attach / 跨实例 fan-out**——CC 单进程单终端。
- **异步审批 UX**(task 11.3 的 web 审批)——CC 是终端同步弹窗。
- **S3/MinIO 工作区后端**(task 6.4)——CC 直连本地 fs。
- **服务端自己的分布式 tracing/metering/计费**(15.x 的多租户维度)——CC 是 CLI 本地 + 发往 Anthropic。

## 跨文档共性主题(反复出现的 CC 智慧)

1. **"把错误建模为 tool_result,而不是致命异常"**——malformed tool call、权限拒绝、工具异常都回灌 `is_error` 让模型自纠,循环不死。(agent-loop / tool-runtime)
2. **"tool_use/tool_result 配对是硬约束,层层设防"**——按 API-round 切分 + ensureToolResultPairing 发送前修复 + abort 时补合成结果。(context-management / agent-loop)
3. **"渐进披露省 token"**——skill L0/L1/L2、工具 defer_loading、memory 只注入 description+按需召回。(skill-system / tool-runtime / memory)
4. **"压缩后必须主动重注入上下文,否则模型变傻"**——文件/plan/skill/goal/工具 schema 压缩后重建。(context-management / skill-system / memory)
5. **"阈值是分层缓冲,不是单点"**——warning/error/blocking/autoCompact 多档,预留输出空间,随 model 走。(context-management / provider)
6. **"重试要分类,错误要归因"**——按状态码分可重试/致命,429/529 特殊处理,prompt-too-long 触发压缩而非失败。(provider / agent-loop)
7. **"per-task 用便宜模型"**——主循环用强模型,摘要/搜索/压缩用小模型,省成本。(model-routing / context-management / dreaming)

## 行动项路线图(跨文档聚合,按优先级)

### P0 — 正确性/安全地基(不做会出 bug 或安全事故)
- **[agent-loop] provider 层重试**(exp+jitter,按状态码分类)——目前 Stream 出错即 run 失败。
- **[agent-loop] malformed tool call 兜底**——JSON 解析失败应产出 is_error tool_result,而非静默 dispatch。
- **[agent-loop] 取消时补配对 tool_result**——否则历史留下孤儿 tool_use,下轮被 API 拒绝。
- **[tool-runtime] 输入 schema 校验**——dispatch 前校验 args,失败转 IsError。
- **[execution-permission] 把 Checker 接入 agent loop 工具分发点**——目前只在测试里被调用,等于没有门。
- **[sandbox] egress-proxy(task 16.1)+ fail-closed**——`NetworkAllowlist` 目前 fallback 到 bridge,形同虚设,最大安全诚信缺口。
- **[sandbox] Exec 超时 + 资源上限**——无超时/内存/CPU/PID 限制,多租户 DoS 向量。
- **[provider] 模型元数据表**(context window + max output)——压缩/MaxTokens 目前用假想值,易超窗。
- **[provider] prompt-too-long 检测**——触发压缩而非 fail run。

### P1 — 核心能力补齐(功能闭环)
- **[context-management] 4.4 全套**:工作视图/produced 分离 + 按 turn 切分 + ensureToolResultPairing + LLM 压缩器 + 压缩后重注入 memory/skill。
- **[tool-runtime] 内置文件/命令/web 工具(沙箱语义)+ MCP seam**(task 10.5)。
- **[tool-runtime] 结果持久化预览** + **并发安全分级**。
- **[execution-permission] 异步审批生命周期**(Ask→持久化 Approval→WS 推送→裁决→恢复 run)+ 审计落库(task 11.3)。
- **[agent-loop] max_tokens 截断恢复** + **steering(转向/插话)** + **usage/stop_reason 观测**。
- **[provider] 消息级缓存断点**(最后一条消息)+ **count_tokens API** + **thinking budget**。
- **[model-routing] per-task 模型分流** + **Meter 成本归属**。
- **[memory] 在线向量召回(接 embedder)或 description+LLM 选择** + **staleness 提醒 + 去重**。
- **[dreaming] 接入 cmd/server** + **触发闸门** + **LLM 矛盾检测**。
- **[skill] SkillTool(运行时加载 L1)+ ScriptTool 注册进 loop**(skill 当前"看得见用不了")。

### P2 — 规模化/精细化(做大后再说)
- **[observability] 15.x 全套**(OTel span 模型 + 成本计量 + replay 视图)。
- **[model-routing] 两级配额 + 限流**(task 14.3,自研)。
- **[agent-loop] 子代理/fork** + **上下文压缩管线**。
- **[provider] cache-break 检测** + **流式→非流式回退** + **529 模型回退**。
- **[tool-runtime] 延迟加载(ToolSearch)**。
- **[skill] PG 持久化版本 + override 审查流**(D16,差异化优势)。
- **[workspace] 6.3 闲置重激活还原** + **6.4 S3 seam**。
- **[session-runtime] fork 会话**(产品能力,PG 上实现)。

## 明确不要从 CC 抄的(信任模型/架构不符)
- **seatbelt/bwrap 沙箱**(隔离强度低于我们的 Docker)。
- **交互式审批弹窗**(无人服务器不适用)。
- **本地文件系统假设的工具实现**(Read/Edit/Bash 深绑本地 fs/shell,100K+ 行 shell 安全解析)。
- **Anthropic SDK 当 canonical 模型**(nowhere 的中立抽象更好)。
- **JSONL 转录 / resume-重建 / 异步 flush**(比 nowhere 的 PG + 在线 replay 粗糙,是倒退)。
- **OAuth/PKCE 登录流**(服务端自签 token 的正确形态我们已经有了)。
- **GrowthBook/Statsig feature-flag 体系**、**快模式/订阅层级判定**(Anthropic 产品策略)。
