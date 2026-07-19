# model-routing vs Claude Code

> 我们的能力:`internal/routing`(多租户 key 解析 + provider fallback)。
> CC 对应:`src/utils/model/`(模型选择)+ `cost-tracker.ts` + `withRetry.ts`(fallback)。

## 我们的现状

`internal/routing/` 刻意保持极简(两文件、四概念):

- **`Router`**(`router.go:40-64`):持有 `provider.Registry`、`KeyStore`、有序 provider `fallbacks`。`Resolve(ctx, userID)` 先调 `keys.Resolve` 拿凭证,再按 `fallbacks` 顺序返回**第一个在 registry 里可用的 provider**。注意:fallback 是**启动期/静态的**——只看 provider 是否注册,不看运行时健康度;也没有按任务类型(summary/embedding/main)区分模型的逻辑。`Target.Model` 字段在 `Resolve` 里**根本没被赋值**(`router.go:60` 只填 `Provider` 和 `Credentials`)。
- **`PGKeyStore`**(`pgkeystore.go:26-45`):多租户核心。SQL 直连,把 `team_api_keys` 与 `team_memberships` join,按 `user_id` 找团队 key(确定性取最小 team_id);没有团队 key 回退到 platform key。`Credentials` 带 `TeamID`/`Platform` 标记用于计费归属。
- **缺口**:`Model` 字段未填;没有 per-task 模型分流;没有成本计量(设计提到 `Meter` 未实现);**Task 14.3 两级配额 + 限流未做**。

## Claude Code 的做法

CC 是**单用户 CLI**:一个 key、一个主循环模型选择、一次会话。它的"路由"实质是**模型解析 + 少量按任务类型用小模型**,而非多租户路由。

1. **主循环模型选择优先级**(`model.ts:92-98`):会话内 `/model` 覆盖 > 启动 `--model` flag > `ANTHROPIC_MODEL` env > settings > 内置默认。内置默认按**订阅层级**分流:Max/Team Premium 给 Opus,其余给 Sonnet(`model.ts:178-201`)。
2. **小快模型**(`model.ts:36` `getSmallFastModel`):`ANTHROPIC_SMALL_FAST_MODEL` env 或默认 Haiku。用于大量廉价任务——session 搜索、离开摘要、token 估算、WebSearch 轻量路径、prompt/agent hooks、skill 改进。
3. **多模型策略(按任务路由)**:CC **确实**按任务分流,但不是显式路由器,而是**调用点各自硬编码**:主循环→`getMainLoopModel()`;摘要/标题/搜索/分类→`getSmallFastModel()`;子代理→`getAgentModel()`(默认 inherit 父模型,可指定,有防降级保护)。**没有 embedding 模型路由**(CC 不做 embedding)。
4. **凭证/认证**(`auth.ts:156-209`):**单 key、单用户**,按优先级从多来源取一个 token(apiKeyHelper > env > OAuth FD > keychain refresh token)。**没有任何"按用户/团队选 key"的概念**——进程级单凭证。
5. **失败回退**:
   - **模型回退(overload)**:连续 529 达 `MAX_529_RETRIES=3` 且配置了 `fallbackModel` 时抛 `FallbackTriggeredError`(`withRetry.ts:337-352`);`query.ts:909-944` 捕获后换 `fallbackModel` 重试整个请求。只对非订阅用户的 Opus 主模型默认启用。
   - **流式→非流式回退**:529/404 时降级到非流式请求。
   - **429/限流重试**:指数退避 + jitter;后台任务(摘要等)遇 529 **直接放弃不重试**(`shouldRetry529`),避免容量雪崩时放大流量。
6. **成本追踪**(`cost-tracker.ts` + `modelCost.ts`):`MODEL_COSTS` 按 canonical 短名硬编码每模型定价(input/output/cacheWrite/cacheRead/webSearch);`calculateUSDCost` 换算 USD;`addToTotalSessionCost` **按模型**累计 token 与花费,存进 project config 供 `/cost` 用。**纯进程内内存累计 + 落盘本地 config,无服务端强制。**
7. **限流/配额**:**几乎不做客户端配额**。429 只是被动重试(订阅用户甚至被明确告知不要重试)。配额强制完全在**服务端**(Anthropic 按订阅层级限流)。**这是与 nowhere 的最大分歧点。**
8. **模型元数据**:上下文窗口(默认 200K,`[1m]` 后缀/beta 提 1M)、最大输出(默认 32K/上限 64K)、per-provider 模型字符串映射表。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 主模型选择 | 优先级链 /model>--model>env>settings>默认,按订阅层级 | 无模型选择;`Target.Model` 未赋值 | 在 Router 加模型解析,填 `Model` |
| 小快模型 | `getSmallFastModel()` env 覆盖→Haiku | 无 | 加 per-task 模型:summary/search 走便宜模型 |
| 按任务分流 | 调用点硬编码(主循环/摘要/子代理) | 无 | Router 加 task-type 维度 |
| 凭证来源 | 单 key,env/helper/OAuth/keychain | **多租户**:team key override else platform | 已领先;保持 |
| 失败回退 | 连续 3×529 切 fallbackModel | 仅启动期 provider 顺序,不看运行时健康 | 加运行时 error→provider/model failover |
| 成本计量 | per-model token→USD,内存+落盘 | 无(设计有 Meter 未实现) | 实现 Meter,按 Credentials.TeamID 归属 |
| 限流/配额 | 几乎无,被动重试,服务端强制 | **Task 14.3 未做** | 做两级配额(platform+team)——CC 无对应物 |
| 上下文/输出元数据 | 200K/1M 窗口、32K/64K 输出 | 无 | 建模型元数据表 |

## 差距与行动项

**单用户 vs 多租户的根本分歧**:CC 的所有"路由"都是**进程内、单凭证、单模型选择**;它把配额/限流完全推给服务端。nowhere 是**平台即服务**,必须自己做多租户 key 解析、成本归属、配额强制——这些 CC **没有对应实现**,不能照抄,只能自研。CC 可借鉴的是它的**模型解析链**和**per-task 小模型分流**这两个纯客户端机制。

1. **【高】补 `Target.Model` 与 per-task 模型分流**:nowhere 的 Router 目前只选 provider 不选 model,且没有任务类型概念。借鉴 CC `getSmallFastModel()`/子代理分流的思路,给 Router 加 task-type → (provider, model) 映射(main loop 用强模型,summary/搜索用便宜模型)。这是 CC 有而 nowhere 完全缺失的最有价值机制。
2. **【高】实现 Meter + 成本归属**:CC 的 cost-tracker 按模型累计 token→USD。nowhere 需要等价物,但关键差异是要按 `Credentials.TeamID`(team key)或 platform 归属计量,落到 PG 而非本地 config,为多租户计费/配额供数。
3. **【高】Task 14.3 两级配额 + 限流**:CC 此处**无实现可抄**(它依赖服务端)。需自研 platform 级 + team 级两级配额与限流。这是 nowhere 多租户本质决定的、与 CC 的最大分歧,也是最大工作量。
4. **【中】运行时 failover 升级**:nowhere 的 fallback 是静态 provider 顺序。借鉴 CC 的 529→fallbackModel 与流式→非流式降级,把 failover 从"启动期是否注册"升级为"运行时错误驱动的 provider/model 切换"。
5. **【中】模型元数据表**:建 per-model 上下文窗口/最大输出/定价表(对应 CC `context.ts`/`modelCost.ts`),供路由选择、成本计量、自动压缩使用。
6. **【低】模型解析优先级链**:CC 的 `/model>flag>env>settings>默认` 链可作为 nowhere 每团队/每用户模型偏好覆盖的参考,但 nowhere 默认应以团队配置 + 平台默认为主,优先级设计与 CC 单用户场景不同。
