# sandbox vs Claude Code

> 我们的能力:`internal/sandbox`(`SandboxPort` + 真实 Docker 实现,多租户)。
> CC 对应:`@anthropic-ai/sandbox-runtime`(seatbelt/bwrap)+ `src/utils/Shell.ts` + BashTool 安全层。**信任模型根本不同。**

## 我们的现状

nowhere 的沙箱是 `Port` 接口 + 真实 Docker 实现,为多租户服务器设计:

- **接口抽象**(`port.go:59-77`):`Port` 暴露 `Create/Destroy/Exec/ReadFile/WriteFile/ListDir`。注释说明"gVisor 或 Firecracker 后端可无感替换"——为多租户隔离强度演进预留。
- **网络策略内建**(`port.go:13-31`):`NetworkPolicy{Mode, AllowedHosts}`,三档 `NetworkOpen/NetworkAllowlist/NetworkDeny`,注释强调"Go 层检查无法阻止容器内代码自行拨号,必须在容器层强制"。
- **Docker 实现**(`docker.go`):每 session 一个容器,工作区 bind-mount 进 `/workspace`,`Cmd: sleep infinity` 长驻,`Exec` 用 `ContainerExecCreate/Attach` + `stdcopy.StdCopy` 分流 stdout/stderr。文件读写走 tar 流。
- **生命周期管理**(`manager.go`):`Manager.Ensure` 按需创建,`MarkSessionEnded` 进入 deferred-stop 宽限期,`Sweep` 由调度器回收过期沙箱。状态机 `running → stopped(可恢复) → destroyed`。
- **关键缺口**:
  - `dockerNetworkMode`(`docker.go:219-233`):`NetworkAllowlist` 目前是 **TODO,直接 fallback 到 `bridge`**(自承 fail closed 最安全但返回了 bridge)。**egress-proxy(task 16.1)未实现——allowlist 形同虚设,这是最大的诚信缺口。**
  - `Exec` 无超时上限、无输出大小看门狗、无后台进程概念。
  - 无内存后端真实隔离(`mem.go` 仅测试桩)。

## Claude Code 的做法

CC 不在容器里跑命令——它用 `@anthropic-ai/sandbox-runtime` 在**用户本机**做 OS 级沙箱,外面再包一层"权限审批"系统。

1. **命令执行/进程 spawn**(`src/utils/Shell.ts:182`):每次执行新建 shell 进程。stdout/stderr 在 file 模式直接写到输出文件 fd(O_APPEND 原子交错),pipe 模式走 StreamWrapper。命令被组装成 `source snapshot && eval <quoted> && pwd -P >| cwdfile`。
2. **超时**:默认 30 分钟;超时触发 `#handleTimeout`——若允许 auto-background 则转后台,否则 SIGTERM。**没有硬上限 concept,超时即杀或转后台。**
3. **输出捕获/流式**:TaskOutput 缓冲 + 轮询文件尾。**大小看门狗**:后台任务轮询输出文件尺寸,超过 `MAX_TASK_OUTPUT_BYTES` 就 SIGKILL(注释提到"768GB incident"教训)。
4. **sandbox-runtime 隔离**:平台相关——**macOS 用 seatbelt(内置),Linux/WSL 用 bubblewrap + socat + 可选 seccomp**。`wrapWithSandbox` 把命令包进 bwrap/seatbelt。可选 seccomp BPF 过滤 syscall。
5. **文件系统限制**:`convertToSandboxRuntimeConfig` 从 settings/permission 规则构建 `allowWrite/denyWrite/denyRead/allowRead`。默认允许写 `.` 和 temp 目录,**强制 denyWrite 所有 settings.json、`.claude/skills`、bare-git-repo 的 HEAD/objects/refs/hooks/config**(防 git 沙箱逃逸 CVE)。执行前还有 AST 静态分析判断命令触碰哪些路径。
6. **网络 egress**:`network.{allowedDomains, deniedDomains, allowUnixSockets, allowLocalBinding, httpProxyPort, socksProxyPort}`。**通过 socat 起一个 HTTP/SOCKS 代理**,沙箱内进程的流量被路由到代理,代理按域名 allowlist 过滤;`SandboxAskCallback` 在命中未授权域名时回调询问用户。**这是真正的 egress proxy,正是我们 task 16.1 缺的。**
7. **危险命令检测**:两层。审批层(`bashSecurity.ts` AST 解析、绕过检测)+ 提示层(`destructiveCommandWarning.ts` 一组正则:`git reset --hard`、`rm -rf`、`DROP TABLE` 等,注释明确"纯信息性,不影响权限逻辑")。**真正的安全边界是权限 prompt + 沙箱,不是正则。**
8. **进程生命周期**:`tree-kill` 杀整棵进程树。`cleanupAfterCommand` 清理 bwrap 留下的挂载点。
9. **per-session 隔离**:**没有**。沙箱是进程级的,配置全局(单例)。grep `per-session/multi-user/tenant` 零命中。

## 信任模型差异

**这是整篇对比的核心:CC 和 nowhere 的威胁模型根本不同,导致 CC 的大多数机制对我们不适用。**

| 维度 | Claude Code | nowhere-agent |
|---|---|---|
| 运行位置 | 用户自己的机器,CLI 进程 | 多租户服务器 |
| 被沙箱的代码 | 用户自己想让 AI 跑的命令 | **不可信的多用户提交的代码** |
| 威胁模型 | "帮用户别误伤自己"(防 `rm -rf ~`、防 AI 乱写 settings) | "隔离敌对租户"(防逃逸、横向移动、资源耗尽、egress 滥用) |
| 隔离强度 | seatbelt/bwrap——**同内核 namespace 隔离**,逃逸面大 | Docker 容器——已是较强隔离,还可升级 gVisor/Firecracker |
| 审批 | 弹窗问用户"允许吗"(有人在场) | **无人在场**,必须预设策略,不能靠交互式审批 |
| 失败语义 | 沙箱不可用时 `allowUnsandboxedCommands` 可 fallback 到非沙箱 | 必须 fail-closed,泄漏一个租户即事故 |

**结论:CC 的 seatbelt/bwrap 隔离强度 < 我们的 Docker,对我们是降级;它的"交互式审批"在无人的服务器上根本不成立。我们该抄的不是它的隔离机制,而是它少量可移植的工程细节。**

## 机制对比表

| 机制 | Claude Code | nowhere 现状 | 行动 |
|---|---|---|---|
| 隔离基元 | seatbelt(mac)/bwrap+socat+seccomp(linux) | Docker 容器 | **不抄**。我们的 Docker 更强。保留 gVisor/Firecracker 演进路径 |
| 网络 egress | socat 起 HTTP/SOCKS 代理,按域名 allowlist 过滤 | **allowlist 是 TODO,fallback 到 bridge** | **必抄思路(不抄实现)**:实现 task 16.1 egress-proxy,让 NetworkAllowlist 真正生效 |
| 危险命令检测 | 权限 AST 分析 + 正则警告 | 无(执行权限门在别处) | 部分可参考:静态分析命令的写路径/网络意图,作为**预设策略**而非交互审批 |
| 超时 | 30min 默认,超时杀或转后台 | **Exec 无超时** | **抄**:给 Exec 加 ctx deadline + 上限 |
| 输出大小看门狗 | 轮询输出文件,超限 SIGKILL | 无 | **抄**:防单租户写爆磁盘/内存 |
| 进程树清理 | tree-kill | 容器 stop/remove 即天然清理 | **已优于 CC**。容器销毁即全清 |
| 文件系统 allowlist | 配置生成 allowWrite/denyWrite | 容器内整盘可写,工作区 bind-mount | 可参考:把 denyWrite 语义映射成容器内 read-only mount |
| per-session 隔离 | 无(全局单例) | **有**,每 session 一容器 + deferred-stop | **我们独有,CC 没有**。保持 |
| 失败语义 | 可 fallback 非沙箱 | NetworkAllowlist fallback 到 bridge | **必须改 fail-closed**:代理未就绪时拒绝而非开放 |

## 差距与行动项

按优先级,**诚实标注哪些 CC 机制不可移植**:

**P0 — 补齐我们自己架构的缺口(与 CC 无关,但必须修)**
1. **egress-proxy(task 16.1)**:`dockerNetworkMode` 目前对 `NetworkAllowlist` 返回 `bridge`,等于放任出站。CC 用 socat 代理证明了这条路可行——我们应建一个 Docker 内部网络 + egress 代理容器,按 `AllowedHosts` 过滤。这是 nowhere 当前**最大的安全诚信缺口**。同时改为 **fail-closed**:代理未就绪时拒绝创建,而不是静默开 bridge。
2. **Exec 超时 + 资源上限**:`Exec` 无超时、无内存/CPU/PID 限制。多租户下这是 DoS 向量。加 ctx deadline,并在 `HostConfig` 设 `Resources`(Memory/NanoCpus/PidsLimit)。CC 的超时和大小看门狗是现成参考。

**P1 — 可移植的 CC 工程细节**
3. **输出大小看门狗**:抄思路,限制 `Exec` 捕获的 stdout/stderr 上限,防单租户写爆。
4. **只读 mount 映射**:参考 CC 的 `denyWrite` 语义,把容器内非工作区路径设为 read-only mount,收敛写面。

**P2 — 明确不抄(信任模型不符)**
5. **不抄 seatbelt/bwrap**:隔离强度低于我们已有的 Docker,是同内核 namespace 方案,对多租户不够。
6. **不抄交互式审批**:CC 的权限弹窗依赖"有人在场"。nowhere 是无人服务器,危险命令检测只能做**预设静态策略**(如拒绝对 NetworkPolicy 外的网络调用、限制特权命令),不能做成运行时询问。
7. **不抄"沙箱不可用时 fallback 非沙箱"**:单用户场景可接受,多租户场景一个租户逃逸即事故,必须 fail-closed。

**根本判断**:CC 的沙箱是"在用户自己机器上给 AI 套个缰绳",重点是防误操作 + 提升体验;nowhere 的沙箱是"在共享服务器上隔离敌对租户",重点是强隔离 + 资源治理 + egress 控制。两者的**隔离层不可互换**,但 CC 在 egress 代理、超时、输出治理上的具体工程做法值得搬进我们的 Docker 实现。
