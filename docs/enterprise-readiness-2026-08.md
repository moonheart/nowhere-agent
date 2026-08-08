# nowhere-agent 企业级就绪评估(治理 / 安全合规 / 可观测 / 部署运维视角)

> **日期**:2026-08-08
> **代码基线**:`master` @ `58ac6e4`(openspec 13 个 change 全部归档,无进行中的 change)
> **范围**:把 nowhere-agent 当作"**企业内部 agent 平台**"看它**能不能合规上线、规模化、可运维**。覆盖十个企业级关注面:①认证授权 ②审计 ③可观测 ④配额限流 ⑤多租户隔离 ⑥部署交付 ⑦管理控制台 ⑧外部集成 ⑨数据治理 ⑩韧性/机密。
> **方法**:三路并行实地探查(运行时接线 / openspec 规格与归档 / 企业就绪逐项),所有结论以代码 `file:line` 为据。
> **姊妹文档**:`docs/backend-architecture-review-2026-07.md`(韧性/多实例/安全视角)、`docs/agent-capability-gaps-2026-07.md`(agent 功能完备性视角)、`docs/claude-code-comparison/00-overview.md`(对标 Claude Code 的机制差距)。本文聚焦"企业级非功能性"这一此前三份文档都未系统覆盖的层面。

---

## 0. 总体判断

**核心的"agent 能力"已经相当完整且工程质量很高,但"企业级"所要求的治理、安全合规、可观测、部署运维这一层基本还没开始。**

前两份评审分别回答了"骨架稳不稳"(架构评审)和"agent 能不能干活"(能力缺口)。结论是:作为**内部 agent 平台的功能面**,覆盖已经比较全面 —— 自建循环、provider 抽象、会话运行时、记忆+dreaming、技能、子代理、通用中断、调度、沙箱、三层管理控制台均已落地并归档。**这不是短板。**

真正的缺口在于"企业级"特有的**非功能性硬功夫**。一句话:**它今天是一个功能完整的内部平台 MVP,离能扛住企业生产环境的"企业级"还差一层治理与运维。**

缺口按对生产落地的阻塞程度分三档:

- **🔴 P0 合规基线** —— 不上这些就没法合规上线(审计、机密加密、可观测)。
- **🟠 P1 规模化** —— 多团队/多租户一上来立刻撞墙(配额执行、SSO、部署交付)。
- **🟡 P2 健壮性/集成** —— 影响信任边界与外部协同(隔离、通用集成、数据治理、多实例)。

> **P0 实施进度(2026-08-08 起,已完成):**
> - [x] **审计日志(§2.2)** —— 已落地:migration `000022` + `internal/audit` + 认证/管理/凭据埋点 + `GET /api/admin/audit`(控制台查看页 `/admin/platform/audit`)。
> - [x] **密钥静态加密(§2.10/§2.5)** —— 已落地:`internal/secrets`(AES-256-GCM)+ `SECRETS_MASTER_KEY` + 渐进迁移 + 轮换。
> - [x] **可观测性 metrics + request-id(§2.3)** —— 已落地:`internal/observability`(/metrics + request-id 关联中间件 + /healthz 依赖探测)。
>
> **P1 实施进度(2026-08-08 起,进行中):**
> - [x] **成本核算(§2.4/§2.1,P1-3)** —— 已落地:`runs` 加 `team_id`/`model`(migration `000023`),团队用量从近似变精确,`usage.ByModel` 打底 per-model 成本。
> - [x] **配额执行 + 请求限流(§2.4,P1-1)** —— 已落地:`internal/quota` + `usage_budgets` 表(migration `000024`),月度 token 预算提交前拦截(429) + per-IP 请求限流中间件(`HTTP_RATE_LIMIT_*`,默认关)。
> - [ ] **OIDC 登录(§2.1,P1-2)** —— 未做:对接 IdP(钉钉/企业微信/飞书走标准 OIDC/OAuth2)。

> 标记说明:✅ 已实现 / ◐ 已实现但未接线或仅部分 / ○ 完全缺失。⭐ = 高价值优先。

---

## 1. 已就绪的能力(基线)

这一层按 openspec 已实现并归档 13 个 change(`openspec/changes/archive/`),完成度很高。作为对照,先列出**不是短板**的部分:

| 能力 | 状态 | 关键实现 |
|---|---|---|
| 自建 think→tool→think 循环 + 中间件链 | ✅ | `internal/agent/loop.go`,权限/压缩/溢出重试中间件,熔断器,max_tokens 恢复 |
| Provider 抽象(Anthropic/OpenAI 双适配) | ✅ | `internal/provider`,thinking 签名回传、prompt cache、图片物化 |
| 会话运行时(RunRegistry 解耦、单活跃 run、多客户端 attach、断连恢复、取消传播) | ✅ | `internal/session`,Redis broker 可选 |
| 持久化(messages 全块存储、流式 broker、历史重建) | ✅ | migration `000006`,`internal/session` |
| 上下文压缩(工作视图/持久分离、轮次切分、配对修复、413 回退) | ✅ | `internal/contextmgmt` |
| 记忆系统 + Dreaming 离线巩固(增量水位、4 阶段、保真规则、有界存储、手动触发) | ✅ | `internal/memory` + `internal/dreaming` |
| 技能系统(L0/L1/L2 渐进披露、三级 scope、版本化) | ✅ | `internal/skill` |
| 子代理(spawn_agent、嵌套流式渲染、递归回放、深度/预算护栏) | ✅ | `internal/subagent` |
| 通用中断原语(审批 / ask_user / 客户端工具统一挂起-恢复) | ✅ | `internal/session/interaction.go`,`internal/agent/loop.go` |
| 调度任务(cron 触发、无人值守权限白名单) | ✅ | `internal/schedule` + `internal/scheduleapi` |
| 沙箱(off/local/docker 三档、网络默认 deny、资源限额) | ✅ | `internal/sandbox` |
| 管理控制台(自助/团队/平台三层:用户/团队/密钥/用量/记忆/技能) | ✅ | `internal/adminapi`,`web/src/components/admin` |

**判断:作为"内部 agent 平台"的功能面,覆盖已经比较全面。下面十节才是企业级的真缺口。**

---

## 2. 企业级就绪逐项评估

> 每一项给出:✅/◐/○ 判定 + 代码证据 + 缺什么。

### 2.1 认证授权 AuthN/AuthZ —— ◐ PARTIAL

**已有:**
- 邮箱+密码认证,bcrypt;不透明 bearer token,SHA-256 哈希存储,30 天 TTL(`internal/identity/service.go:30-61,123-126`)。
- 超越 admin/member 的 RBAC:团队三角色 `owner > admin > member` 带等级排序(`internal/identity/types.go:73-108`)+ 正交的二级 `PlatformRole`(`user|admin`,migration `000016`)。
- 服务端按路由强制团队 scope(`internal/adminapi/handler.go:70-102` 的 `requireTeamRole`、`guards.go`);资源可见性经 `Service.AccessibleScopes`(用户+团队+系统)过滤(`internal/identity/service.go:95-106`)。
- 停用账户即吊销 token(`internal/identity/store_admin.go:100-133`);`BOOTSTRAP_ADMIN_EMAIL` 引导管理员。

**缺失:** 无 SSO/OIDC/SAML/OAuth(全树仅 docs 与 go.sum 命中);无 MFA;无密码策略;无面向程序化访问的服务 API key;无 token TTL/scope 配置;无登录节流/锁定。

> **企业影响:** 内部接入几乎必然要对接 IdP(OIDC/LDAP)。这是 ◐ 而非 ○,因为本地 RBAC 本身做得相当扎实。

### 2.2 审计日志 Audit —— ✅ **已实现(2026-08-08)**

> **状态更新:已由缺口转为落地。** `internal/audit`(append-only Logger + Event 构建器 + List 查询)+ migration `000022_audit_log` + 埋点(identity 认证事件 + adminapi 全部管理/凭据/记忆写路径)+ 平台管理员查询端点 `GET /api/admin/audit`。
>
> **已实现:**
> - 追加式 `audit_log` 表(migration `000022`):actor(可空,删除账号不带走轨迹)/action/outcome/target/ip/ua/detail(jsonb);actor_email 反范式快照,账号删除后轨迹仍可读。
> - `internal/audit`:`Logger.Log`(INSERT,无 update/delete 路径)+ fluent `Event` 构建器(结构上无 secret 字段);`ClientIP` 优先 X-Forwarded-For 最左一跳(适配反代/网关部署)。
> - **best-effort 语义**:`LogAndReport` 写失败仅落 server log,绝不影响被审计动作本身 —— 审计轨迹永不成为动作的单点故障。
> - **埋点**:登录/注册/登出(含失败,记录尝试邮箱而非密码)、改密、token 撤销、平台账户管理(建/改/停/启/重置密码/删/改角色)、团队管理(建/改名/删/成员增删改角色)、**团队 provider key 设/删(只记轮换发生,绝不记密钥材料)**、记忆删/弃用。
> - **查询**:`GET /api/admin/audit?action=&actor=&from=&to=&limit=&offset=`,仅平台管理员可读。
> - **控制台查看页**:admin 控制台 Platform 区新增 **Audit trail** 页(`web/src/components/admin/AuditPage.tsx`,路由 `/admin/platform/audit`)—— action 下拉(枚举服务端固定 action 集)/actor id/起止日期筛选 + 分页表格,只读(轨迹本为 append-only,不提供任何变更入口)。typed client 在 `web/src/lib/admin.ts`(`listAudit` + `AuditEntry`/`AUDIT_ACTIONS`)。

**遗留(非阻塞):** 工具执行(tool_execution)类事件未纳入 —— 工具调用由 agent loop 内部派发,纳入需在 loop 层埋点,留作后续增量。聊天内容本身刻意不记(隐私与存储成本)。

### 2.3 可观测性 Observability —— ✅ **已实现(2026-08-08,P0-3)**

> **状态更新:已由缺口转为落地。** `internal/observability`(Prometheus metrics + request-id 关联中间件 + 真实健康探测)+ 接线于 `cmd/server/main.go`。
>
> **已实现:**
> - **Metrics**:`internal/observability.Metrics` 持独立 `prometheus.Registry`(不污染全局,测试/多次构造不冲突);`GET /metrics` 暴露 `nowhere_http_requests_total{route,method,status}`、`nowhere_http_request_duration_seconds{route,method}` 直方图、`nowhere_http_inflight_requests` 表。基数有界:route 标签取 ServeMux 模式 `r.Pattern` 而非裸路径 —— `/api/users/{id}` 无论多少 id 都并成一条序列,未匹配路径归 `unmatched`,`metrics.Middleware` 包住整个 mux(最外层之下)。
> - **Request-id 关联**:`observability.RequestID` 为最外层中间件;采信外部 `X-Request-Id`(仅当短且可打印,拒绝注入控制字符)否则 crypto/rand 生成 128-bit hex;回写响应头 + 注入 `FromContext`/`LoggerFromContext`(请求级 logger 自动带 request_id/method/path,下游链路日志免费带关联)。
> - **真实健康探测**:`observability.Healthz`,`/healthz` 只在所有依赖探针通过时回 200(取代原先无条件 "ok");探针并发执行、各受 2s 超时约束,挂起依赖不会拖垮健康检查;任一失败回 503 并点名依赖。接线:启动即挂 postgres(`pool.Ping`),选 redis broker 时挂 redis。
>
> **仍缺(非阻塞,后续批次):** tracing(`go.opentelemetry.io/otel` 仍是间接依赖,源码未 import)、pprof。架构留有挂点:`Metrics` 已预留 `nowhere_runs_total`/`nowhere_llm_tokens_total` 计数器,待 run 生命周期与 usage 记账埋点后启用。

> **注意:** `openspec/specs/observability/spec.md` 与 `docs/claude-code-comparison/observability.md` 仍为**纯设计文档**;本次实现独立于它们,以 `internal/observability` 为运行真相。

### 2.4 配额 / 限流 / 成本控制 —— ◐ PARTIAL ⭐

**已有(只记账,不执行):**
- Token **记账**:per-call/per-run 用量列(migration `000013`),读侧聚合 `internal/usage/store.go`(按账户/团队/日期;仅 token,刻意不做成本估算)。
- 子代理单次 run 扇出上限:`WithBudget(maxTotal, maxConcurrent)`(`internal/subagent/tool.go:83-86`,env `SUBAGENT_MAX_TOTAL/MAX_CONCURRENT`)。
- Dreaming worker 单次 token 预算 + 按类记忆上限(`internal/dreaming/worker.go:54-55`)。
- Docker 沙箱资源限额:512 MiB / 1 CPU / 256 PID(`internal/sandbox/docker.go:32-36`)。

> **配额执行 / 请求限流:已实现(2026-08-08,P1-1)。** 此前用量"只记账零执行",一个账户可以烧穿共享团队 key;现在两道口子都会咬人,且都 **fail-open**(配额库/限流器出问题绝不拖垮 chat):
> - **月度 token 预算拦截**:`internal/quota` + `usage_budgets` 表(migration `000024`,scope=user/team、`owner_id` TEXT 无 FK、`monthly_tokens>0`,PK `(scope,owner_id)`)。`quota.Checker` 在 **run 提交前**(任何模型调用之前)比对"本月可计费 token(input+output) ≥ 月度上限",命中即拒绝:chat 路径返回 **HTTP 429 + Retry-After**(`WithBudgetGate`,映射 `quota.ErrBudgetExceeded`);scheduled run 同样拦截(命中则**跳过本次触发、保持 due** 下个扫描重试,记 warn 日志而非报错整个 sweep)。预算按**自然月**(UTC 月初边界,`monthWindow`,中国企业对账按月结)窗口;用户预算与付费团队预算双重检查,任一命中即拒。spend 走 `usage.Store.ForUser/ForTeam` 的薄 adapter(billable=`Tokens.Total()`),enforcement 与读侧报表解耦(`SpendFunc`/`BudgetReader` 接口)。
> - **请求限流**:`quota.RateLimiter`(per-key 令牌桶,`golang.org/x/time/rate`),挂为最外层中间件(`httpHandler`,在 request-id/metrics **之前**——拒绝洪流时不为一个将被 429 的请求浪费任何 per-request 工作或 metric);429 + `Retry-After: 1`。key 默认按客户端 IP(`ClientIPKey`,取 X-Forwarded-For 首跳、剥离源端口),`/healthz`/`/metrics` 探测**豁免**(洪流时监控不能瞎)。env `HTTP_RATE_LIMIT_RPS`/`HTTP_RATE_LIMIT_BURST` 控制,默认 0=**关闭**(本地/dev 不受限),两者皆设才启用;空桶后台 sweeper 回收(TTL 10min)。
> - 设计要点:配额是**保护预算**的手段,绝不让平台因配额库抖动而 429 自己;预算执行在提交时 fail-open,真正超支才 429。预算的**配置 UI**(控制台配额页)尚未补——后端 CRUD(`Store.Set/Get/Clear`)已就绪。

**缺失(剩余):** 配额的**管理面**(控制台配额配置页)未补;无 per-route/分级限流。

> **成本核算 / 精确团队用量:已实现(2026-08-08,P1-3)。** `runs` 加 `team_id`/`model` 两列(migration `000023`),run 提交时按**实际付费的团队 key**(`routing.Resolve(...).TeamID`)与 loop 配置的 model 打戳(`RunWork.TeamID/Model` → `SetRunAttribution`;chat 走 `TeamAttributor`,scheduled run 同样打戳):
> - **团队用量从近似变精确**:`internal/usage` 的 team 维度改读 `runs.team_id`(直接归属),跨团队成员不再重复计数、离职成员不再带走历史;`team_id` 可空且**不带 FK**,删团队不删历史 run,NULL = 平台 key 付费。
> - **历史行渐进兼容**:打戳前的旧行(team_id NULL)回退到"按当前成员"的旧近似(单条 OR + EXISTS 子句),随时间精确占比趋近 100%;`TeamOverlapNote` 更新为"新数据精确、旧数据近似"。
> - **per-model 成本打底**:新增 `usage.Store.ByModel`(按 model 聚合),`runs.model` 使成本核算不再需要猜 model;挂载每模型定价即可把 token 换算成钱(定价属配置,不在 store 内)。
> - 设计要点:attribution 在**提交时**服务端解析(chat 用 `TeamAttributor` 解析付费团队;error → 平台,绝不阻塞 run),**尽力而为**(打戳失败仅记日志)。

### 2.5 多租户隔离 —— ◐ PARTIAL

**已有:**
- 逻辑租户模型:users/teams/memberships;技能与记忆打 user/team/system scope 并在召回/建工具时按 `AccessibleScopes` 过滤(`cmd/server/main.go:453-472`)。
- 团队级 LLM 凭据:`team_api_keys` 表 + 每请求解析 + 平台 key 回退(`internal/routing/pgkeystore.go:34-53`,`cmd/server/main.go:713-741`);控制台遮蔽显示(`MaskKey`)。
- 会话归用户所有;沙箱 per-session(独立容器或独立宿主目录)。

**缺失:** 单一共享 Postgres + 租户列,但**无行级安全(RLS)** —— 隔离完全压在应用层 WHERE 子句上;MCP 是全平台共享一个 client,无 per-tenant 沙箱/凭据隔离;无 per-team 工具策略;无租户级加密;团队 API key **明文存储**(见 2.10)。

### 2.6 部署交付 Deployment —— ◐ PARTIAL ⭐

**已有:**
- 迁移工具 `cmd/migrate`(golang-migrate),21 个编号 up/down migration。
- 优雅退出:SIGINT/SIGTERM + `srv.Shutdown` + 可配超时(`cmd/server/main.go:64,660-668`)。
- CI:`.github/workflows/test.yml` 三系统(ubuntu/windows/macos)build/vet/test。
- env 驱动配置;Redis Streams/Pub-Sub broker 提供多实例雏形(`cmd/server/main.go:124-137`)。

**缺失:** 无 Dockerfile、无 docker-compose、无 k8s/Helm —— **没有任何可部署产物**;CI 只跑测试,无 web lint、无镜像构建/发布、无 release workflow;仓库根目录提交了 Windows 二进制(`server.exe`/`migrate.exe`/`mockllm.exe`),呈"本地 dev 优先"姿态;无就绪/存活探针分离(仅一个浅层 `/healthz`)。

### 2.7 管理控制台 Admin console —— ✅ IMPLEMENTED(就现有后端而言)

控制台在 `web/src/components/admin/`,路由 `web/src/App.tsx:383-395`,后端路由面 `internal/adminapi/handler.go:54-103` 与之一一对应:

- **自助:** 资料/改密/token 管理(`ProfilePage`)、我的用量/记忆/dream 触发(`SelfPages`)、我的技能+版本/回滚编辑器(`SkillsPages`+`SkillEditor`)、调度任务(`ScheduledTasksPage`)。
- **团队:** 团队列表/详情多 tab —— 改名、成员增删改角色、团队 provider key(设/删/遮蔽)、团队用量、团队记忆。
- **平台(admin):** 用户(列/建/改/重置密码/停/删)、团队(列/为属主建)、用量、记忆(删/弃用)、技能。

> **说明:** 控制台的三层管理 UI 与后端一一对应,且**审计查看页已补**(P0-1,`/admin/platform/audit`)。控制台**仍没有**配额配置、部署/运维页 —— 因为对应后端尚不存在(见 2.4/2.6)。控制台的能力上限由后端决定。

### 2.8 外部集成 Integrations —— ◐ PARTIAL

- **MCP:** `internal/mcp/client.go` 仅 Streamable HTTP(无 stdio/SSE),且**硬编码单个内置 SearXNG**(`NewSearxng`,固定 `searxng` 前缀,env `MCP_SEARXNG_URL`)。**没有通用的多 server MCP 配置。** 后台退避重连,工具 per-run 注册(`cmd/server/main.go:507-512`)。
- **webhook/Slack/邮件/通知:** ○ **全缺**(grep webhook/slack/notification 无命中)。调度任务跑完结果只能留在 session,无外发渠道。
- **外部连接器:** 除 LLM provider(anthropic/openai)与 SearXNG 外全无;无 Slack bot、无 GitHub/Jira 连接器、无 inbound webhook 触发。

### 2.9 数据治理 Data governance —— ◐ PARTIAL

**已有:**
- 记忆保留:弃用记忆经 `DREAMING_PURGE_AFTER`(默认 30d)清除(`cmd/server/main.go:327`);记忆模型有软弃用→遗忘生命周期(`internal/memory/port.go:38`)。
- 硬删用户:`DELETE FROM users`(`internal/identity/store_admin.go:169-170`)+ admin 路由 `DELETE /api/admin/users/{id}`。

**缺失:** 无 GDPR 式数据导出(无任何 export 端点);**会话/消息/run 永久保存**,无对话数据保留策略;无 PII 检测/脱敏;用户删除靠 FK 级联,无逐用户数据清单/已验证擦除流程;无数据分类/DLP。

### 2.10 韧性 / 机密 Resilience & secrets —— ◐ PARTIAL ⭐

**已有:**
- 沙箱隔离三档 `off|local|docker`;docker = per-session 容器 + 工作区 bind-mount + 网络策略 `deny|open|allowlist`(默认 deny,allowlist fail-closed)+ 资源限额(`internal/sandbox/docker.go:20-36`);local 限定宿主目录,`run_command` 由 `SANDBOX_LOCAL_EXEC` 门控(文档注明仅限可信单租户)。
- 风险分级工具权限门:按 risk class allow/deny/ask(`internal/permission`,接线于 `cmd/server/main.go:227-282`)+ per-session 覆盖 + 人工审批流。
- 启动搁浅 run  reconciliation;MCP 故障降级而非启动失败;团队 key 解析失败回退平台 key。
- 认证 token 哈希存储;密码 bcrypt。

> **密钥静态加密:已实现(2026-08-08,P0-2)。** 团队 provider API key 不再明文入库:
> - `internal/secrets`:AES-256-GCM 认证加密,随机 nonce 逐值前置(同值两次加密不同);密文自描述 `enc:v1:<base64(nonce||ct)>`。
> - **主密钥来自 env** `SECRETS_MASTER_KEY`(raw 32B 或 base64;自托管单二进制的可信根)。未设置则回退明文 + 启动告警,**绝不静默**(误配的 key = 硬失败)。
> - **渐进迁移**:读者靠前缀区分密文/明文,旧明文行照常读、下次写时重加密 —— 启用加密非 flag day。
> - **密钥轮换**:密文头带 key id + 有序 key ring,新 key 加密、旧 key 仍可解密旧密文;`internal/routing` 读写路径透明加解密,console mask 取自明文(非密文尾巴)。
> - **故障语义**:无法解密的行 → Resolve 响亮报错(回退平台 key + 记日志),绝不向 provider 静默发送损坏凭据。

**仍缺:** 主密钥本身无 KMS/Vault 集成(当前 env 即可信根,威胁模型见 `internal/secrets` 包注释;可经 KeySource 抽象接 KMS 而不改信封格式);LLM 平台 key 是裸 env var;无备份工具/脚本/文档化策略;除 MCP 重试外无熔断;Docker 沙箱依赖宿主 Docker socket 的信任边界代码未讨论。

---

## 3. 汇总记分卡

| # | 关注面 | 判定 | 关键缺口 |
|---|---|---|---|
| 1 | 认证授权 | ◐ | 本地 RBAC 扎实;无 SSO/MFA/服务 API key |
| 2 | 审计日志 | ✅ | 已落地(migration `000022` + `internal/audit` + 埋点 + 查询 API) |
| 3 | 可观测 | ✅ | 已落地(/metrics + request-id 关联 + /healthz 依赖探测);tracing/pprof 仍缺 |
| 4 | 配额限流 | ◐ ⭐ | 只记账零执行;无成本核算 |
| 5 | 多租户 | ◐ | 仅应用层 scoping;无 RLS;~~团队 key 明文~~(已加密,P0-2) |
| 6 | 部署交付 | ◐ ⭐ | 迁移+优雅退出+CI 测试;无容器/k8s 产物 |
| 7 | 管理控制台 | ✅ | 自助/团队/平台三层管理 UI 广泛 |
| 8 | 外部集成 | ◐ | 单个硬编码 SearXNG MCP;无 webhook/通知 |
| 9 | 数据治理 | ◐ | 记忆 TTL + 硬删;无导出、对话无保留策略 |
| 10 | 韧性/机密 | ◐ ⭐ | 沙箱分档与故障降级好;~~无静态机密加密~~(已加密,P0-2)、无备份 |

**最大的企业级缺口:** ~~审计轨迹(2)~~、SSO(1)、~~metrics/tracing(3)~~、配额执行(4)、~~所存 provider key 的静态加密(5/10)~~、任何可部署产物(6)。(P0 三项均已落地;剩余为 P1 起。)

---

## 4. 已知技术债(此前评审已标记,按预期未做)

以下为 `backend-architecture-review-2026-07.md` 与 `agent-capability-gaps-2026-07.md` 已编号记录、尚未落地的项,与本评估的企业级缺口互补:

- **C17 ◐** local 沙箱 `run_command` 的 `sh -c` 未转义模型参数 → 本地后端宿主 RCE 风险;**阻塞 K3b 技能脚本执行**。
- **L6 / routing.Router ◐** 多模型 failover 类型存在(`internal/routing/router.go`)但生产未接线(单 provider 硬选,`cmd/server/main.go` 仅经 `buildProvider` 建单适配器)。
- **A1 ○** goroutine 派发点无 `recover()` —— 一个工具 panic 可拖垮整个多租户进程。
- **A2 / A4 ○** provider 层无重试;畸形 tool-call 无输入 schema 校验、被静默派发。
- **K4 ○** embedder 未做,向量召回(RecallVector)尚无在线 embedder 供给。
- **多实例 HA ◐** Redis broker 在,但跨实例 EventBus、run 放置/亲和性仍是 follow-up(文档明注"多实例未建")。

---

## 5. 建议落地批次

按"合规基线 → 规模化 → 可交付 → 健壮性"推进,每批相对内聚、可独立成 openspec change:

| 批次 | 内容 | 对应缺口 | 规模估计 |
|---|---|---|---|
| **批次 1 合规基线** | 审计日志(表+中间件+写路径埋点);团队 key 静态加密(pgcrypto/KMS);metrics + request-id(先 `/metrics` 与关联,再上 tracing) | 2.2 / 2.10 / 2.3 | 2–3 周 |
| **批次 2 规模化** | 配额/限流执行(per-user/team token 预算拦截 + 请求限流);OIDC 登录;成本核算(`runs` 加 `team_id`/`model` 列,顺带解决团队用量近似问题) | 2.4 / 2.1 | 2–3 周 |
| **批次 3 可交付** | Dockerfile + compose + CI 出镜像 + 就绪/存活探针分离 | 2.6 | 1–2 周 |
| **批次 4 健壮性/集成** | C17 沙箱转义;A1 recover;provider 重试;通用多 server MCP + webhook 通知 | 2.8 / §4 | 按需 |

> **建议的切入点:** 批次 1 中的**审计日志**或**密钥静态加密** —— 这两个是企业合规的硬门槛,且改动相对内聚(前者一张表+中间件+埋点,后者一个加密层+密钥来源抽象),适合各自成为一个独立的 openspec change。
