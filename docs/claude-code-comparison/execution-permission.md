# execution-permission vs Claude Code

> 我们的能力:`internal/permission`(风险等级裁决)+ 待办 **11.3**(审批 UX + 审计日志)。
> CC 对应:`src/hooks/useCanUseTool.tsx` + `src/utils/permissions/` + `src/hooks/toolPermission/`。

## 我们的现状

nowhere 的执行权限模型非常精简,核心是**基于风险等级的静态策略映射**,而非规则引擎:

- `internal/permission/permission.go`(~101 行):
  - `Verdict`(`Allow`/`Ask`/`Deny`)是运行时裁决;`Decision` 是策略配置值,一一对应。
  - `Policy` 按 **4 个风险等级**配置:`ReadOnly`/`SandboxWrite`/`Network`/`ExternalWrite`。`DefaultPolicy()`:沙箱内 allow,逃逸类(网络、外部写)ask。
  - `Checker.Check(t Tool)`:**仅读取 `t.Risk()` 一个字段**映射到 Verdict。不看工具名、入参内容、命令文本、路径。
  - `Approval` 结构体已定义(`ID`/`RunID`/`ToolName`/`Detail`),是 11.3 的占位,**但全库无任何代码引用它**(仅测试)。
- `internal/toolruntime/tool.go`:`Risk` 枚举是 permission 的唯一输入,由每个 Tool 自我声明。
- **关键缺口**:
  1. `Checker` **未接入任何执行路径**——只在测试里被调用。即"决策"存在但无"门"。
  2. `session.RunWaitingApproval` 状态已存在(`internal/session/types.go`),异步审批的 run 状态机骨架已就位,但 `Ask` verdict 落到哪、谁创建 `Approval`、如何恢复 run 都没实现(即 11.3)。
  3. 无规则系统、无权限模式、无 hooks、无危险命令/路径启发式、无审计日志。

## Claude Code 的做法

### 1. 决策流:`CanUseToolFn` → `hasPermissionsToUseTool`
- 签名(`src/hooks/useCanUseTool.tsx:46`):`CanUseToolFn(tool, input, ctx, assistantMessage, toolUseID, forceDecision?) → Promise<PermissionDecision>`。
- 决策形状(`src/types/permissions.ts:241-324`):`Allow | Ask | Deny`。
  - `Allow`:`{behavior:'allow', updatedInput?, userModified?, decisionReason?}`——可**改写入参**。
  - `Ask`:`{behavior:'ask', message, suggestions?: PermissionUpdate[], blockedPath?, ...}`——**suggestions 就是"always allow"的候选规则**。
  - `Deny`:`{behavior:'deny', message, decisionReason}`。
  - `decisionReason` 是 12 种 tagged-union:`rule|mode|subcommandResults|permissionPromptTool|hook|asyncAgent|sandboxOverride|classifier|workingDir|safetyCheck|other`——完整记录"为什么",可解释、可审计。
- 管线顺序(`src/utils/permissions/permissions.ts:1160` `hasPermissionsToUseToolInner`):
  1a 整工具 deny → 1b 整工具 ask → 1c `tool.checkPermissions(input)` 工具自检 → 1d 自检 deny → 1e 需交互 → 1f 内容级 ask(bypass 也不豁免)→ 1g safetyCheck(bypass 免疫)→ 2a bypass 放行 → 2b 整工具 allow → 3 passthrough 转 ask。
- 外层再做模式变换:`dontAsk` 把 ask→deny;`auto` 模式跑 YOLO 分类器;headless 跑 PermissionRequest hook 否则 auto-deny。

### 2. 权限模式
`src/types/permissions.ts:16-38` + `PermissionMode.ts`:
- `default`:只读自动放行,写/危险问。
- `acceptEdits`:自动接受工作区内文件编辑。
- `plan`:只读规划,禁写。
- `bypassPermissions`:全放行,但 1d deny 规则、1f 内容 ask、1g safetyCheck 仍生效(**bypass 免疫的硬规则**)。
- `dontAsk`:ask→deny。`auto`(内部 feature):分类器代替人问。

### 3. 规则系统
- 规则 `PermissionRule{source, ruleBehavior, ruleValue}`;字符串形式 `Tool(content)`,如 `Bash(npm run:*)`。`Bash()`/`Bash(*)` 视为整工具规则。
- 三类规则集 `alwaysAllow/Deny/AskRules`,按 source 分桶。
- 匹配:整工具 `toolMatchesRule`(支持 `mcp__server__*` 通配);内容级 `getRuleByContentsForTool` 交给工具自检做前缀/模式匹配。
- **优先级:deny > ask > allow**;source 顺序 `policySettings > cliArg > ... > user/project/localSettings`;`policySettings`(企业管控)只读不可删。存于 settings.json。
- `PermissionUpdate` 操作集:addRules/replaceRules/removeRules/setMode/addDirectories…,可持久化。

### 4. 审批 prompt UX
- `interactiveHandler.ts`:把 `ToolUseConfirm` push 进 React 队列,带 `onAllow/onReject/onAbort/recheck` 回调。
- `createResolveOnce.claim()`(`PermissionContext.ts:75`)提供原子一次性裁决,让**本地对话、bridge(claude.ai 网页)、channel(Telegram/iMessage)、PermissionRequest hook、bash 分类器**五方竞争同一裁决,先到先得。
- 选项(`bashToolUseOptions.tsx`):`yes` / `yes, and don't ask again for <prefix>`(把命令前缀持久化成 allow 规则)/ `yes-apply-suggestions` / `no`(可带反馈)。
- "always allow" 持久化:`onAllow` → `persistPermissions(updates)` 写 settings.json;日志区分 `permanent` vs `temporary`。

### 5. Hooks(PreToolUse/PostToolUse/PermissionRequest)
- PreToolUse hook 可返回 `permissionDecision: allow|deny|ask` + reason;`behavior:'allow'` 短路放行。
- `PermissionRequest` hook 在审批弹出时(交互)或 headless 时跑,可 allow(含改 input + 持久化)或 deny(可 `interrupt` 中止整个 agent)。
- PostToolUse 在执行后跑,可 block/给反馈。

### 6. 危险命令/路径启发式 → ask
- **Bash**:`src/tools/BashTool/bashSecurity.ts`(2593 行!)是一整套校验器链,覆盖命令替换、反引号、heredoc 注入、git `-m` 注入、jq `system()`、重定向、换行/CR 解析差异、IFS 注入、`/proc/*/environ`、ANSI-C quoting、brace expansion、Unicode 空白等。命中即 ask,部分标 `isBashSecurityCheckForMisparsing` 提前硬阻断。
- **路径**:`src/utils/permissions/filesystem.ts` 列 `DANGEROUS_FILES`(`.bashrc`/`.gitconfig`/`.mcp.json`)与 `DANGEROUS_DIRECTORIES`(`.git`/`.vscode`/`.claude`),大小写归一防 `.cLauDe` 绕过。命中返回 `safetyCheck`——bypass 免疫。

### 7. 审计/日志
- 唯一入口 `logPermissionDecision`(`src/hooks/toolPermission/permissionLogging.ts:181`),所有 approve/reject 都过它。扇出:Statsig analytics(按 source 分事件名 `tengu_tool_use_granted_in_config/_by_classifier/_in_prompt_permanent/_temporary`、拒绝 `tengu_tool_use_denied_*`)、OTel `tool_decision`、内存 `toolDecisions` 供下游查询。`waiting_for_user_permission_ms` 记录人等了多久。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 决策输入 | 工具名+入参内容+命令文本+路径+模式+规则+hook | **仅 `Tool.Risk()` 一个枚举** | 扩展 Check 输入,至少加工具名 |
| 决策形状 | `Allow(updatedInput)/Ask(message,suggestions)/Deny(reason)` + 12 种 reason | 裸 Allow/Ask/Deny,无 reason/suggestions | Verdict 携带 reason/message;Ask 携带建议规则 |
| 规则系统 | allow/deny/ask 三类,`Tool(content)` 模式,deny>ask>allow | **无**,只有 4 级静态 Policy | 设计 `Tool(pattern)` 规则 + 来源优先级 |
| 权限模式 | default/acceptEdits/plan/bypass/dontAsk/auto | **无** | 至少加 default/bypass/plan |
| 接入执行门 | `CanUseToolFn` 在每个 tool_use 前 await | **未接入**,Checker 只在测试用 | **在 agent loop 工具分发处调用 Checker** |
| 危险命令启发式 | bashSecurity 数十校验器 + 危险文件/目录清单 | **无**(逃逸类一律 ask) | 按 Risk 等级先做最小命令/路径分析 |
| 审批交互 | CLI 弹窗,5 方竞争,同步等待 | Approval 结构体已定义未用;`RunWaitingApproval` 已在 | **异步**:WS/SSE 推 Approval,HTTP 裁决后恢复 run |
| "always allow" | `yes-prefix-edited` 持久化 `Bash(prefix:*)` | 无 | 审批通过可选持久化为 allow 规则 |
| Hooks | Pre/Post/PermissionRequest,可 allow/deny/ask/改 input | 无 | 可选,P2 |
| 审计 | `logPermissionDecision` 单入口,扇出 analytics+OTel+内存 | 无 | 决策日志落库(含 reason/来源/耗时) |

## 差距与行动项

**核心架构差异(CLI 同步 vs Web 异步)**:CC 的 `CanUseToolFn` 返回**同步 await 的 Promise**——人坐在终端前,Promise 一直 pending 直到按键。nowhere 是 B/S 多用户后端,审批必须**异步跨 HTTP**:agent loop 在 `Ask` 时不能阻塞 goroutine 等人,而应 ① run 置 `RunWaitingApproval`(状态已就绪)② 持久化 `Approval` ③ WS/SSE 推给对应用户 ④ 收到 `POST /approvals/{id}/{approve|deny}` 后恢复 run。这正是 11.3。CC 的 `createResolveOnce.claim()`(多消费者竞争同一裁决)思路可借鉴——nowhere 也要防"审批已被另一客户端/超时处理"的重复裁决。

**P0(11.3 前置,缺一不可)**
1. **把 Checker 接入 agent loop 工具分发点**——目前只在测试里被调用,等于没有门。
2. **异步审批生命周期**:`Ask` → 创建并持久化 `Approval`(结构体已存在)→ run 置 `RunWaitingApproval` → WS 推送 → 收到裁决 → 恢复/中止 run。加超时 + `claim()` 式一次性裁决防重。
3. **审计日志落库**:记录 tool、run、verdict、来源(policy rule/用户临时/用户永久)、耗时。对齐 `logPermissionDecision`。

**P1**
4. **Ask 携带建议规则(suggestions)**:批准时可选"总是允许",持久化为 allow 规则。需先有规则系统。
5. **规则系统**:`Tool(pattern)` allow/deny/ask + 来源优先级(deny>ask>allow)。最小可先支持整工具名与命令前缀匹配,不必一开始就做 bash AST 分析。
6. **权限模式**:至少 default/bypass/plan。注意 CC 里 deny 规则与 safetyCheck 对 bypass 免疫——nowhere 若加 bypass 也要保留"不可绕过"的硬规则。

**P2**
7. **危险命令/路径启发式**:现在逃逸类一律 ask,体验差但安全。可逐步引入危险文件/目录清单(`.git`/`.bashrc`)与最小命令黑名单,把明显危险的从 ask 升级为"必须人审且不可 auto-allow"。
8. **Hooks**:多用户平台可用 Webhook/策略插件实现,供管理员注入组织级 allow/deny(对应 CC 的 `policySettings` 只读企业管控)。
9. **decisionReason tagged-union**:把"为什么裁决"结构化,喂给审计与前端解释 UI。

**不照搬**:bashSecurity.ts 那 2593 行 shell 解析是针对"在用户自己机器上跑命令防误伤"的,nowhere 跑的是沙箱内多租户代码,威胁模型不同——我们要防的是**逃逸**(网络/外部写),不是**误伤**。先做 Risk 等级 + 逃逸类必审,shell AST 解析大部分不需要。
