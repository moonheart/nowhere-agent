# tool-runtime vs Claude Code

> 我们的能力:`internal/toolruntime`(统一 Tool 接口 + Registry,task 10.5 内置工具+MCP 未做)。
> CC 对应:`src/Tool.ts`(抽象)+ `src/tools/`(内置)+ `src/services/tools/`(执行)+ `src/services/mcp/`(MCP)+ `ToolSearchTool`(延迟加载)。

## 我们的现状

`internal/toolruntime/` 是一个**最小但设计正确的统一抽象**(task 10.5 未实现):

- **Tool 接口**(`tool.go:36-49`):`Name/Description/Schema() map[string]any/Risk()/Timeout()/Call(ctx, args) (Result, error)`。
- **Result**(`tool.go:29-33`):`{Content, IsError}`,错误以 `IsError=true` 回喂模型自我纠正。
- **Risk 枚举**(`tool.go:14-25`):`RiskReadOnly/RiskSandboxWrite/RiskNetwork/RiskExternalWrite`——为多用户沙箱后端设计的权限维度,**比 CC 更贴合我们的场景**。
- **Registry**(`registry.go`):`map[string]Tool` + 锁;`Call` 派发并套用超时(默认 30s);未知工具和 error 都转成 `IsError` Result;`CallAll` 用 goroutine 并发执行并保持顺序。
- 有完整表驱动单测。

**现状结论**:契约层(Tool 接口 + 统一 Result + 并发派发 + 超时)已就位且测过。**缺**:schema 校验、权限判定、结果截断/持久化、内置工具实现、MCP seam、延迟加载。

## Claude Code 的做法

### 1. Tool 抽象(`src/Tool.ts`,~30KB 类型文件)
CC 的 `Tool` 是一个**超宽的对象字面量类型**,把执行、权限、schema、UI 渲染、遥测全揉在一个接口里:
- **标识/描述**:`name`、`aliases?`、`searchHint?`(延迟加载关键词)、`description(input, opts)`(可按 input 动态生成)。
- **schema**:`inputSchema`(zod);MCP 工具例外,可直接给 `inputJSONSchema`(绕过 zod)。
- **生命周期**:`call(args, context, canUseTool, parentMessage, onProgress?) → ToolResult<Output>`;`ToolResult{data, newMessages?, contextModifier?, mcpMeta?}`。
- **校验/权限三段式**:`validateInput?`(值级校验,schema 只查类型)、`checkPermissions`(工具专属权限)、`preparePermissionMatcher?`(把 input 变成权限规则可匹配的串,如 `Bash(git *)`)。
- **行为谓词**:`isReadOnly(input)`、`isConcurrencySafe(input)`(**并发门控的关键**)、`isDestructive?(input)`、`interruptBehavior?(): 'cancel'|'block'`。
- **结果上限**:`maxResultSizeChars`——每个工具声明自己的结果大小阈值;`Infinity` 表示永不持久化。
- **结果序列化**:`mapToolResultToToolResultBlockParam(content, toolUseID)`——把 Output 转成 API 的 `tool_result` content block。**这是工具层与 API wire 格式的唯一接缝。**
- **大量 UI 渲染方法**(renderToolUseMessage 等)——CLI/TUI 专属,后端不抄。
- **`buildTool` 默认值**:所有工具经 `buildTool(def)` 构造,自动填 fail-closed 默认(`isConcurrencySafe→false`、`checkPermissions→allow` 等)。**默认全部偏保守。**

### 2. 注册与发现;内置 + MCP 合并
- **内置工具清单** `getAllBaseTools()`:硬编码数组(Agent/Bash/FileRead/FileEdit/FileWrite/Glob/Grep/WebFetch/WebSearch/NotebookEdit/TodoWrite/…)+ feature-flag 条件工具。
- **模式过滤** `getTools(permissionContext)`:按 deny 规则剥掉整组工具;按 `isEnabled()` 过滤。
- **合并入口 `assembleToolPool`**:内置 + MCP 合并的单一事实来源。分组各自按 name 排序再拼接,内置在前,`uniqBy('name')` 去重且内置优先(排序为 prompt-cache 稳定)。
- **MCP 判定**:`name.startsWith('mcp__') || tool.isMcp===true`。

### 3. 内置工具集与关键行为/上限
- **FileReadTool**:默认读至多 2000 行;双重上限 `maxSizeBytes=256KB`(查整文件,读前 stat 抛错)+ `maxTokens=25000`(读后抛错,可 env 覆盖)。**超限是抛错而非截断**(注释解释:截断回灌 25K token,抛错只回 ~100 字节,反而省 token)。`maxResultSizeChars: Infinity`。
- **FileEditTool/FileWriteTool**:`maxResultSizeChars: 100_000`。
- **BashTool**:`maxResultSizeChars: 30_000`;`isConcurrencySafe(input)` 按命令内容判定。大 stdout 落盘:超限时写到 tool-results 目录,>64MB 时 truncate 后让模型用 FileRead 读回。配套巨量安全/权限代码(`bashSecurity.ts` 102K、`bashPermissions.ts` 99K)——**CLI 本地 shell 专属,不能照抄**。
- **GrepTool** `20_000`;**GlobTool** `100_000`;**WebFetchTool/WebSearchTool** `100_000`。

### 4. 工具执行管线
调用链:`runTools` → `toolOrchestration`(并发/串行分批)→ `runToolUse` → `checkPermissionsAndCallTool`:
- **输入校验**:`tool.inputSchema.safeParse(input)`。失败拼成 `<tool_use_error>InputValidationError: …</tool_use_error>`,包成 `is_error:true` 的 tool_result 返回,**不中断循环**。
- **顺序**:zod parse → `validateInput?.()` → PreToolUse hooks → 权限 `checkPermissions` → 全部通过才 `tool.call(...)`。
- **结果映射**:`call` 成功后 `mapToolResultToToolResultBlockParam` 转 tool_result block,再做大小检查。
- **并发门控**:`partitionToolCalls` 把连续 tool_use 分批——连续的 concurrency-safe 工具合成一批并发(上限默认 10),非 safe 的单独串行。`isConcurrencySafe` 抛异常时**保守按不安全处理**。
- **错误处理**:`call` 抛错 `formatError`(>1万字符截头 5000+尾 5000)包成 is_error tool_result。未知工具:`No such tool available`。

### 5. 结果截断/大小上限
集中常量 `toolLimits.ts`:`DEFAULT_MAX_RESULT_SIZE_CHARS=50_000`、`MAX_TOOL_RESULT_TOKENS=100_000`、`MAX_TOOL_RESULTS_PER_MESSAGE_CHARS=200_000`(单条 user message 内所有并行 tool_result 的聚合预算)。
**核心机制是"持久化到磁盘"而非"截断"**(`toolResultStorage.ts`):
- `getPersistenceThreshold`:`Infinity` 直接返回;否则 `min(工具声明值, 50K)`。
- `persistToolResult`:超限结果写到 `sessionId/tool-results/<id>.txt`,返回 2KB 预览。
- 模型收到 `<persisted-output>Output too large. Full output saved to: <path>\n\nPreview (first 2KB):…</persisted-output>`,可用 Read 按需读回全量。

### 6. MCP seam
- **模板工具 `MCPTool`**:`buildTool` 构造的占位模板,`isMcp:true`、`inputSchema` passthrough、`maxResultSizeChars:100_000`、`checkPermissions→passthrough`。
- **真实构造 `fetchToolsForClient`**:`tools/list` 后把每个 MCP tool `{...MCPTool, ...覆盖}`。
- **命名**:`buildMcpToolName(server, tool)` → `mcp__<server>__<tool>`,两段经 `normalizeNameForMCP`(非 `[a-zA-Z0-9_-]` 替换为 `_`,满足 API 约束)。**回发给 server 时用原始未归一化的 `tool.name`**。
- **schema 转换**:**不做 zod 转换**,直接 `inputJSONSchema: tool.inputSchema` 透传给模型。
- **annotation 映射**:MCP `annotations` → 工具谓词(`readOnlyHint→isReadOnly/isConcurrencySafe`、`destructiveHint→isDestructive`)。
- **连接**:`@modelcontextprotocol/sdk` 的 Client + StdioClientTransport(也支持 sse/http/ws/sdk)。

### 7. 延迟/懒加载(ToolSearchTool + defer_loading)
- **ToolSearchTool**:只读、并发安全、始终加载。输入 `{query, max_results=5}`。`select:<name>` 直接选 / 关键词搜索打分(name 精确+12、部分+6、searchHint+4、description+2)。
- **激活载荷**:返回 `tool_reference` block 数组,**服务端把 `tool_reference` 展开成完整 schema**。
- **wire 标记 `defer_loading`**:在每个请求上叠加 `schema.defer_loading=true`;`isMcp`→总是 defer;`alwaysLoad`→不 defer。
- **无注册表变异**:发现集是消息历史的函数,每次请求重算。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| Tool 契约 | 超宽接口(执行+权限+schema+UI+遥测) | 窄接口 6 方法,职责干净 | **保持窄接口**,只补 ValidateInput/权限/截断字段,不抄 UI 渲染 |
| Schema 定义 | zod `inputSchema`;MCP 例外用 `inputJSONSchema` 透传 | `Schema() map[string]any` 直接返回 JSON Schema | 我们的方式更接近 MCP 透传,**补入参校验步骤**即可 |
| 输入校验 | zod safeParse 失败→InputValidationError tool_result;再 validateInput 值级 | 无校验,args 直接进 Call | **P0**:dispatch 前加 schema 校验,失败转 IsError |
| 权限判定 | checkPermissions + matcher + 通用权限引擎 | 只有 `Risk()` 枚举,无判定逻辑 | **P0**:基于 Risk 实现审批门(见 execution-permission.md) |
| 并发门控 | `isConcurrencySafe(input)` 决定并行/串行分批(上限 10) | `CallAll` 一律并发,无安全区分 | **P1**:加 `IsConcurrencySafe()` 或用 `Risk==RiskReadOnly` 近似 |
| 结果→wire | `mapToolResultToToolResultBlockParam` 唯一接缝 | `Result{Content,IsError}` 已是 tool_result 形态 | 已对齐,保持 |
| 结果截断 | **持久化到磁盘+2KB 预览**,非截断;阈值 `min(工具声明,50K)`,聚合预算 200K/消息 | 无上限 | **P1**:每工具 MaxResultSizeChars + 写沙箱文件 + 预览 |
| 超时 | `callTool` 带 timeout + abort signal | `Registry.Call` 套 Timeout 默认 30s | 已对齐,保持 |
| 错误处理 | 双层 catch,formatError 截断,is_error tool_result | Call error→IsError Result | 已对齐,保持 |
| 内置工具 | 文件/命令/web 全套,但**深绑本地 fs/shell** | 无 | **P0 实现文件/命令/web,但重写为沙箱后端语义** |
| MCP seam | MCPTool 模板 + `mcp__server__tool` 命名 + JSON Schema 透传 + annotation→谓词 | 无 | **P1**:MCP 客户端 + 命名前缀 + mcpInfo + schema 透传 |
| 延迟加载 | `defer_loading` wire 标记 + ToolSearch + tool_reference 展开 | 无 | **P2**:工具多时再做;依赖服务端 tool_reference 支持 |

## 差距与行动项

### P0(task 10.5 核心,必须先做)
1. **输入校验**:在 `Registry.Call` 派发前,用工具的 `Schema()` 对 `args` 做 JSON Schema 校验,失败返回 `IsError` Result(归为缺参/多参/类型错)。这是当前最大的正确性缺口——CC 注释原话"the model is not great at generating valid input"。
2. **实现内置文件/命令/web 工具**,但**不要抄 CC 的实现**——它们深绑本地 fs/shell。我们要的是沙箱语义:文件工具操作会话工作区(而非任意本地路径)、命令工具在沙箱/exec 后端跑、web 工具走可控 egress。`Risk` 枚举已为此设计好。
3. **权限审批门**:基于 `Risk()` 实现审批判定(见 [execution-permission.md](execution-permission.md))。

### P1(工具可用的关键配套)
4. **结果大小上限 + 持久化预览**:给 Tool 接口加 `MaxResultSizeChars()`,超限结果写入**会话沙箱存储**(而非本地磁盘)并回 `<persisted-output>` 预览。这套"存文件+预览+模型按需读回"的机制非常值得抄,思路与我们的会话沙箱天然契合,只是存储后端要换成 per-session 隔离存储。阈值语义学 `min(工具声明, 50K)`。
5. **并发安全门控**:`CallAll` 目前一律并发。加 `IsConcurrencySafe(args)`(或复用 `Risk()==RiskReadOnly` 做近似),把连续的只读工具并发、写工具串行。CC 的保守默认(`isConcurrencySafe→false`)值得抄。
6. **MCP seam**:实现 MCP 客户端,包成 Tool。直接抄:(a) `mcp__<server>__<tool>` 命名 + 名称归一化;(b) **JSON Schema 直接透传不转**——与我们的 `Schema() map[string]any` 完美契合;(c) MCP `annotations` → `Risk()` 映射(`readOnlyHint→RiskReadOnly`、`openWorldHint/destructiveHint→network/external_write`);(d) `mcpInfo` 保留 server/tool 原名用于权限与回发。

### P2(规模化后再做)
7. **延迟加载**:当工具数量大到拖累上下文时,引入 `defer_loading` + ToolSearch。注意它依赖**服务端把 `tool_reference` block 展开成完整 schema**。可先做简化版:注册表标记 deferred,ToolSearch 命中后把该工具的完整 schema 注入下一轮请求,不依赖服务端 beta。

### 明确不要抄的(CLI 专属)
- **本地文件系统假设**:CC 的 Read/Edit/Write/Glob/Grep 全假设本地 fs、绝对路径、readFileState 缓存、文件历史快照。多用户沙箱后端必须用 per-session 虚拟工作区 + 路径隔离,不能暴露宿主路径。
- **BashTool 的 100K+ 行权限/安全代码**(`bashSecurity.ts`、`bashPermissions.ts`、`pathValidation.ts`)——那是在不可信的本地 shell 上做静态分析的产物。我们的命令工具应在受控沙箱/exec 环境里跑,安全边界由沙箱提供,而非靠解析 shell 命令字符串。
- **UI 渲染方法**(renderToolUseMessage 等)——React/Ink TUI 专属,后端无意义。我们的 Tool 接口不含它们是**正确的**。
- **GrowthBook/Statsig feature-flag 覆盖**——实验基础设施,不需要。
