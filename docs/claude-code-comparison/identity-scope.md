# identity-scope vs Claude Code

> 我们的能力:`internal/identity`(多租户:用户/团队/token/scope)。
> CC 对应:`src/services/oauth` + `src/utils/auth.ts`(**几乎无对应物**)。

## 我们的现状

nowhere 是**多租户服务端**,identity 是一等公民能力,围绕"用户/团队/系统"三级作用域:

- **三级 Scope 模型**(D8):`ScopeUser / ScopeTeam / ScopeSystem`,资源(skill、memory)只挂一个 scope;`ScopeRef` 用 `UserID`/`TeamID` 标识属主(`internal/identity/types.go:8-28`)。
- **账号体系**:自管用户表,email + bcrypt 密码哈希。
- **自签发 Bearer token**:Login 校验后生成 32 字节随机 token,**SHA-256 哈希后落库**(DB 泄露不丢会话),TTL 30 天。
- **HTTP 中间件鉴权**:`RequireAuth` 从 `Authorization: Bearer` 解析 token → 查库得 user → 注入 request context;路由 `/api/auth/signup|login|logout`、`/api/me`。
- **团队成员与角色**:`Membership{TeamID, UserID, Role{owner|admin|member}}`。
- **资源可见性**:`AccessibleScopes(userID)` = 本人 user scope + 所有所属 team scope + system scope,供 skill/memory 过滤召回。

核心定位:**身份/租户/属权是平台的根基,所有资源都按 scope 隔离与授权。**

## Claude Code 的做法(在本维度极度稀疏)

CC 的身份模型是**"把当前这台机器上的这一个用户,认证到 Anthropic API"**。没有任何服务端多租户概念。

1. **OAuth 2.0 + PKCE 登录流**(唯一"登录"机制):`OAuthService.startOAuthFlow` 起 localhost 回调 + PKCE + state 防 CSRF,浏览器跳转授权页(`src/services/oauth/index.ts:32-86`)。这是**客户端消费第三方 IdP**的 flow,不是服务端签发凭证。
2. **API 认证头三选一**:OAuth subscriber 用 `Authorization: Bearer <accessToken>`;console/API-key 用户用 `x-api-key: <key>`(`client.ts:332-336` 二选一)。
3. **Bedrock/Vertex 凭证**:`CLAUDE_CODE_USE_BEDROCK` → AWS 凭证,`CLAUDE_CODE_USE_VERTEX` → GCP ADC。同样是单机环境变量/本地 ADC,无用户概念。
4. **账户/订阅层追踪(仅读、用于限流与遥测,不做授权)**:OAuth profile 返回 org type 映射到 `max|pro|enterprise|team`,连同 `rate_limit_tier`、org/account UUID 写入全局 config。**只决定"用哪档速率/走 Bearer 还是 x-api-key",不参与任何资源访问控制。**
5. **token 存储与刷新(单机本地)**:macOS 用 Keychain,其他退化到 `~/.claude/.credentials.json` 明文 + 0o600;refresh_token grant 续期,401 时清缓存重读。
6. **多用户/属权:完全没有。** 全库搜 `tenant/membership/ownedBy/ownerId` 在业务代码零命中;仅存的 "multi-user" 注释指**同一台 Unix 机器上多个 OS 用户**的临时目录权限隔离;Session/Memory 目录里**无任何 userId/ownerId 字段**——资源只按"这台机器 + 这个项目目录"组织。

## 根本差异(标题结论)

**Claude Code 是单用户 CLI:它把"本机的这一个人"认证到远端 Anthropic API,身份的生命周期、存储、刷新全部围绕"一份本机凭证"。它没有用户表、没有团队成员、没有角色、没有资源属权、没有服务端授权——因为这些在"一个人用一台机器"的场景里根本不存在。**

**nowhere 是多租户服务端:身份/租户/属权是平台的根基能力。** 它要签发并校验自己的 token、在 DB 里维护用户与团队、把每个 session/memory 归属到 user/team/system scope、并在每个请求上做授权。这一套在 CC 里**没有任何对应物**。

两者甚至连"token 从哪来"都方向相反:
- CC 是**下游消费者**——向 Anthropic 这个 IdP 换取/刷新别人签发的 token。
- nowhere 是**上游签发者**——自己哈希密码、自己生成并哈希存储 Bearer token、自己判定 scope 可见性。

因此 CC 在本维度能提供的东西几乎为零,其 OAuth-flow 机制(PKCE、localhost 回调、浏览器跳转)对一个**自己发 token 的服务端毫无借鉴价值**。

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 认证目标 | 本机单用户 → Anthropic API | N 个终端用户 → nowhere 服务端 | 保持服务端模型 |
| token 来源 | 第三方 IdP 签发(消费方) | 自签发 + 自校验(签发方) | 无需借鉴 |
| 登录机制 | OAuth+PKCE+浏览器+localhost 回调 | email+密码 bcrypt,自产 token | CC 流程不适用 |
| 密码/凭证存储 | 无密码;token 存 Keychain/`.credentials.json` 0o600 | bcrypt 密码哈希 + token SHA-256 落库 | 已是服务端正确做法 |
| 用户/团队模型 | **无** | User/Team/Membership/Role | CC 无对应,自建 |
| 角色/授权 | 仅 orgRole 存 config 做遥测 | owner/admin/member + scope 过滤 | CC 无授权语义,自建 |
| 资源属权/scope | **无**(只按机器/项目目录) | user/team/system 三级 scope | 核心差异,自建 |
| 每请求鉴权 | 客户端选 Bearer/x-api-key 头 | `RequireAuth` 中间件 | 已是服务端正确做法 |
| 订阅/账户层 | max/pro/team/enterprise,用于限流 | 无(平台自身不订阅上游) | 概念不迁移 |

## 差距与行动项

诚实结论:**在 identity-scope 维度,Claude Code 对 nowhere 几乎没有可迁移的东西。** 它是单用户 CLI,把一个本机用户认证到 API;nowhere 是多租户服务端,身份/scope/属权是必须独立设计的一等能力,CC 在这里是空白。

- **不要借鉴 CC 的 OAuth flow。** PKCE/localhost 回调/浏览器跳转是"客户端消费 IdP"的模式,与"服务端自签发 token"无关。nowhere 现有的 email+密码+自产 Bearer token 就是正确形态。
- **真正缺失的不是 CC 能补的,而是服务端通用最佳实践**(CC 同样没有,需自行设计):
  1. **Token 刷新/轮换机制**:nowhere 目前是 30 天固定 TTL、logout 即删,无 refresh token / 滑动续期 / 并发撤销语义(CC 反而有成熟的 401 重读与跨进程失效处理 `auth.ts:1319-1374`——可参考其"清缓存重读 + 去抖"的并发思路,但其存储层不适用)。
  2. **基于 Role 的细粒度授权**:`Membership.Role` 已建模,但需确认 skill/memory 的写路径是否真正按 owner/admin/member 做了差异化拦截(`AccessibleScopes` 目前只覆盖读可见性)。
  3. **Session/资源的属权落地**:types 里有 `ScopeRef`,需在 session/memory 的建表与查询层强制带 scope 谓词,防止跨租户串读(CC 完全没有、也无需有的问题)。
  4. **可选的第三方 SSO/OIDC**(企业客户常用):若未来要接 Google/SSO,才需要回头参考 CC 的 orgUUID 参数化——但那是消费方模式,仅当 nowhere 作为 SP 接外部 IdP 时才相关。
- **结论**:本维度 nowhere 应**以自己的多租户设计为准**,把 CC 仅当作"单用户凭证生命周期"的参考,而非身份/授权模型的来源。
