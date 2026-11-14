# 需求追踪矩阵（v0.2.0 现状）

> **生成方式**：逐条对照 `task.md`（150 行，§1–§12）与 `task2.txt`（11 行）到当前实现与测试；
> 所有「实现位置」均来自本轮 `grep`/读码取证（非凭印象），行号为**当前工作区**真实行号。
> 取证基线：v0.2.0 发布提交（tag `v0.2.0`，工作区含阶段 6B/6C/6D/6E 成果）、
> `go1.24.4 windows/amd64`。阶段 6E 只更新 §5 工程增强状态、版本号与 §6 复核结论，
> 不改需求侧判定；§5.2 新增条目全部使用 v0.2.0 真实行号（阶段 6C 重构过的文件已逐条复核）。
>
> **定位约定**：`文件:行` 写成可点击链接（相对 `docs/`），如 `../internal/relay/tcp_data.go#L131`；
> 测试写作 `包/文件:TestName`（链接指向该测试函数起始行）。
>
> **状态只有四种**：
> `✅ 已实现`（有实现且有测试/实测证据）、`🟡 部分实现`（能力在，但不满足该条字面要求）、
> `❌ 缺失`（要求存在、实现不存在）、`🔒 环境受限`（代码已实现，但本机无法验证，如管理员级 Windows 服务）。
>
> **判定纪律**：`task.md` 没要求的工程增强（自动重连、热切换、发送队列、soak、systemd/Docker 等）
> **一律不算需求缺失**，只进 §5 的「工程增强」区，不参与 §1/§2 的状态统计。

---

## 摘要

| 分类 | 条目数 | ✅ 已实现 | 🟡 部分实现 | ❌ 缺失 | 🔒 环境受限 | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| task.md §1–§8（功能与约束） | 59 | 59 | 0 | 0 | 0 | 三模式、配对流程、ini、管理页、安全、日志、命名全部有实现+测试 |
| task.md §9（交付物与 e2e） | 7 | 7 | 0 | 0 | 0 | 源码/README/e2e 五类验收项齐全 |
| task.md §10（验收标准） | 6 | 5 | 0 | 0 | 1 | 唯一受限项是 Windows 服务完整生命周期（需管理员） |
| task.md §11（五个自由度说明） | 5 | 3 | 2 | 0 | 0 | ①client_conn_id 生成方、②管理页 HTML 组织=README 未写 |
| task.md §12（版本/许可） | 2 | 2 | 0 | 0 | 0 | tag `v0.1.0` 与 `v0.2.0` 均存在；Apache-2.0 |
| task2.txt（UDP 承载模式改造） | 6 | 6 | 0 | 0 | 0 | `udp_mode` 三处下发、`carrier`、隧道帧、README 取舍全部落地 |
| **合计** | **85** | **82** | **2** | **0** | **1** | 需求缺口 = 0；未了项 = 2 处 README 说明 + 1 处环境验证 |
| 工程增强（§5.2，不计入需求统计） | 8 | 7 | 0 | 1 | 0 | E1–E6 已落地（阶段 6C/6D）；E8 文档时点已标注；E7（TLS 强制开关）仍未做 |

**一句话结论**：v0.2.0 对 `task.md` + `task2.txt` 的**功能需求已全部实现且可验证**；
剩下的不是「功能缺失」，而是 **2 处 README 应有说明缺失**（§11 的两个自由度）+
**1 处必须管理员环境才能验证的 Windows 服务生命周期**；v0.1.0 时点的 6 项工程增强
（自动重连、热切换、发送队列、UDP 重传、soak、部署件）已在阶段 6C/6D 全部落地（§5.2）。

---

## 1. task.md 逐条

### 1.1 §1 项目概述（4 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §1.1 | 是配对式中继，**不是 P2P NAT 穿透**，必须写在 README 第一句 | [README.md:3](../README.md#L3)（正文第 3 行即该句）；[README.md:17-27](../README.md#L17-L27) 对比表；[main.go:3-5](../main.go#L3-L5) 包注释同表述 | 文档类，无测试（验收靠人工核对 README 首段） | ✅ 已实现 | — |
| task.md §1.2 | 协议无关的字节流中转，一套系统承载 HTTP/RDP/VNC/SSH 任意 TCP/UDP，不解析上层协议 | [internal/relay/tcp_data.go:131-157](../internal/relay/tcp_data.go#L131-L157)（数据面只读一帧 Bind）；[internal/forward/forward.go:115-131](../internal/forward/forward.go#L115-L131)（纯 io.Copy）；[README.md:6-8](../README.md#L6-L8) | [TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)（真实 net/http 零改造）、[TestE2ETCPEchoConcurrent20](../test/e2e_basic_test.go#L184) | ✅ 已实现 | — |
| task.md §1.3 | 交付物是单个可执行文件，用 `-relay/-agent/-client` 子命令切换三种模式 | [main.go:68-73](../main.go#L68-L73)（三个 bool flag）、[main.go:108-127](../main.go#L108-L127)（必须且只能一个）、[main.go:324-335](../main.go#L324-L335)（dispatch） | [TestSelectMode](../main_test.go#L11)、[TestBinaryVersionUsageAndConfigErrors](../test/binary_test.go#L82)（无模式/多模式退出码 2） | ✅ 已实现 | — |
| task.md §1.4 | 同一二进制按子命令加载 ini 对应段；三模式均支持 Linux 与 Windows | [internal/config/config.go:336-351](../internal/config/config.go#L336-L351)（四段全解析）、[internal/config/config.go:238-261](../internal/config/config.go#L238-L261)；[README.md:12](../README.md#L12) | [TestCrossCompile](../test/binary_test.go#L492)（linux/amd64、windows/amd64、windows/arm64）；Linux 真机见 [README.md:862-1061](../README.md#L862-L1061) §13.15 | ✅ 已实现 | 各模式只读自己的段由 `Run` 取 `cfg.Relay/Server/Client` 体现，见 §7.2 行 |

### 1.2 §2 三种模式职责（14 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §2.1 | relay 监听三个业务端口，职责严格分离 | [internal/relay/server.go:101-137](../internal/relay/server.go#L101-L137)（tcp→udp→tunnel 顺序）；[README.md:49-55](../README.md#L49-L55) | [TestRunPortInUse](../internal/relay/cleanup_test.go#L105)、[TestE2EUDPModeNative](../test/e2e_udp_test.go#L18) | ✅ 已实现 | `udp_listen` 自 task2 起可选（见 §T2.1） |
| task.md §2.2 | 隧道端口职责：注册、服务列表查询/订阅、接入请求、回连通知、心跳、错误响应 | [internal/relay/tunnel.go:286-322](../internal/relay/tunnel.go#L286-L322)（dispatch）、[:324](../internal/relay/tunnel.go#L324) 注册、[:378](../internal/relay/tunnel.go#L378) 列表、[:396](../internal/relay/tunnel.go#L396) 接入、[:549](../internal/relay/tunnel.go#L549) 推送、[tunnel.go:667-682](../internal/relay/tunnel.go#L667-L682)（writeError） | [TestTunnelHelloAuth](../internal/relay/tunnel_test.go#L16)、[TestTunnelRegisterListSubscribe](../internal/relay/tunnel_test.go#L90)、[TestTunnelConnectRequest](../internal/relay/tunnel_test.go#L183) | ✅ 已实现 | — |
| task.md §2.3 | TCP 通信端口纯字节流双向转发，校验一次性配对令牌，**不得引入协议解析** | [internal/relay/tcp_data.go:127-157](../internal/relay/tcp_data.go#L127-L157)（一次性读帧）、[:189-192](../internal/relay/tcp_data.go#L189-L192)（Validate）、[:285-287](../internal/relay/tcp_data.go#L285-L287)（io.Copy） | [TestTCPPairAgentFirst](../internal/relay/tcp_data_test.go#L48)、[TestTCPPairClientFirst](../internal/relay/tcp_data_test.go#L72)、[TestTCPBindRejections](../internal/relay/tcp_data_test.go#L95) | ✅ 已实现 | 报到帧之后零解析（§3.7） |
| task.md §2.4 | UDP 通信端口按配对键维护会话映射转发数据报，同样校验令牌 | [internal/relay/udp_data.go:45-140](../internal/relay/udp_data.go#L45-L140)（:53 反查、:98 校验、:108 登记）；[internal/registry/udp_table.go:155](../internal/registry/udp_table.go#L155)、[:224](../internal/registry/udp_table.go#L224) | [TestNativeUDPForwardAndTimeout](../internal/relay/udp_data_test.go#L93)、[TestE2EUDPModeNative](../test/e2e_udp_test.go#L18) | ✅ 已实现 | 该端口现为可选（task2），能力本身完整 |
| task.md §2.5 | 维护连接注册表：服务ID→server 控制连接、配对键→两侧数据连接、待配对表、UDP 会话表；并发安全 | [internal/registry/service_registry.go:51](../internal/registry/service_registry.go#L51)；[internal/registry/agent_registry.go:10](../internal/registry/agent_registry.go#L10)；[internal/pairing/pairing.go:155](../internal/pairing/pairing.go#L155)；[internal/registry/udp_table.go:120](../internal/registry/udp_table.go#L120)；[internal/relay/carrier.go:23](../internal/relay/carrier.go#L23)（over-TCP 会话表）；[internal/relay/tcp_data.go:373-398](../internal/relay/tcp_data.go#L373-L398)（活动配对） | [TestServiceRegistryConcurrent](../internal/registry/registry_test.go#L177)、[TestUDPTableConcurrent](../internal/registry/registry_test.go#L484)、[TestConcurrentPairings](../internal/pairing/pairing_test.go#L267)、全仓 `-race` 见 README §13.15.2 | ✅ 已实现 | 服务→控制连接经 `agent_id` 间接索引（[:735](../internal/relay/tunnel.go#L735)），功能等价 |
| task.md §2.6 | agent 连 relay 隧道，声明服务ID、协议类型（tcp/udp）、本地目标地址 | [internal/agent/control.go:73-128](../internal/agent/control.go#L73-L128)（Hello）、[:129-159](../internal/agent/control.go#L129-L159)（逐个 RegisterService）；[internal/config/config.go:157-168](../internal/config/config.go#L157-L168) | [TestHelloAndRegisterAllServices](../internal/agent/agent_test.go#L13)、[TestRegisterServiceError](../internal/agent/agent_test.go#L110) | ✅ 已实现 | — |
| task.md §2.7 | 维持控制长连接，等待接入通知 | [internal/agent/reconnect.go:138](../internal/agent/reconnect.go#L138)（`serveControl`：唯一读循环 + 自动重连）、[internal/agent/control.go:306](../internal/agent/control.go#L306)（心跳） | [TestControlConnectionLost](../internal/agent/agent_test.go#L220)、[TestReconnectAfterControlDrop](../internal/agent/reconnect_test.go#L52)、[TestHeartbeatRebuiltAfterReconnect](../internal/agent/reconnect_test.go#L244) | ✅ 已实现 | 阶段 6C 起断开后**自动重连**（E1）；`reconnect=false` 可回退"断开即退出" |
| task.md §2.8 | 配置维护多组 `服务ID → tcp/udp:本地端口` 映射，本地第三方服务零改造 | [internal/config/config.go:400-449](../internal/config/config.go#L400-L449)（service.<ID> 解析）、[:157-168](../internal/config/config.go#L157-L168)（主服务 default 在前）；[README.md:350-362](../README.md#L350-L362) | [TestLoadValidAgent](../internal/config/config_test.go#L116)、[TestHelloAndRegisterAllServices](../internal/agent/agent_test.go#L13)、[TestE2EAgentMultiServiceConcurrent](../test/e2e_basic_test.go#L278) | ✅ 已实现 | 支持 `proto:port`、`proto:host:port`、IPv6 方括号 |
| task.md §2.9 | 收到通知后携带配对键+令牌**新建连接**到对应协议通信端口完成绑定 | [internal/agent/agent.go:388-449](../internal/agent/agent.go#L388-L449)（handleIncoming 按 proto 分派）、[internal/agent/data.go:53-115](../internal/agent/data.go#L53-L115)（TCP 报到） | [TestTCPIncomingBindAndForward](../internal/agent/data_test.go#L18) | ✅ 已实现 | — |
| task.md §2.10 | 绑定后在回连 relay 的数据连接与到本地目标的连接之间双向转发 | [internal/agent/data.go:53-114](../internal/agent/data.go#L53-L114)；[internal/forward/forward.go:83-145](../internal/forward/forward.go#L83-L145) | [TestTCPIncomingBindAndForward](../internal/agent/data_test.go#L18)、[TestE2ETCPEchoConcurrent20](../test/e2e_basic_test.go#L184) | ✅ 已实现 | — |
| task.md §2.11 | client 连隧道，获取服务列表（启动拉取 + 可选订阅变更） | [internal/client/control.go:74-91](../internal/client/control.go#L74-L91)（ListServices）、[:242-299](../internal/client/control.go#L242-L299)（控制读循环/订阅）；[internal/client/views.go:45-108](../internal/client/views.go#L45-L108) | [TestServiceListSubscribeEvents](../internal/client/client_test.go#L93)、[TestSubscribeRevisionResync](../internal/relay/subscribe_resync_test.go#L66) | ✅ 已实现 | — |
| task.md §2.12 | 为配置中要访问的服务，在本机开对应本地监听端口（TCP/UDP） | [internal/client/listener.go:52-112](../internal/client/listener.go#L52-L112)（按 relay 列表 proto 选 TCP/UDP） | [TestRunHandshake](../internal/client/client_test.go#L28)、[TestNativeUDP](../internal/client/udp_test.go#L81)、[TestPortConflictReturnsError](../internal/client/client_test.go#L214) | ✅ 已实现 | 列表缺失时按 TCP 占位并 WARN（README §5.6） |
| task.md §2.13 | 第三方连本地端口时发起接入请求，走完整配对流程，在本地连接与中继数据连接间双向转发 | [internal/client/listener.go:149-183](../internal/client/listener.go#L149-L183)、[:204-263](../internal/client/listener.go#L204-L263)；[internal/client/control.go:323-391](../internal/client/control.go#L323-L391)；[internal/client/session_tcp.go:171-229](../internal/client/session_tcp.go#L171-L229) | [TestTCPEndToEnd](../internal/client/tcp_test.go#L19)、[TestE2ETCPEchoConcurrent20](../test/e2e_basic_test.go#L184) | ✅ 已实现 | — |
| task.md §2.14 | 第三方客户端无需改造，只连 `127.0.0.1:本地端口` | [internal/client/listener.go:100-109](../internal/client/listener.go#L100-L109)（`net.Listen` 于配置地址）；[README.md:364-375](../README.md#L364-L375)（默认 127.0.0.1） | [TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)（标准 `http.Client` 只连本地端口） | ✅ 已实现 | — |

### 1.3 §3 核心配对流程（11 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §3.1 | agent 注册服务，relay 记录 `服务ID → 控制连接` | [internal/relay/tunnel.go:324-375](../internal/relay/tunnel.go#L324-L375)（写 ServiceRegistry + SetServices） | [TestTunnelRegisterListSubscribe](../internal/relay/tunnel_test.go#L90) | ✅ 已实现 | — |
| task.md §3.2 | client 请求访问；relay 生成 `客户端连接ID`、`配对键 = (服务ID, 客户端连接ID)`、一次性令牌 | [internal/relay/tunnel.go:396-502](../internal/relay/tunnel.go#L396-L502)（:460 Issue 令牌、:474/:486 下发）；[internal/client/client.go:396](../internal/client/client.go#L396)（conn_id 生成方） | [TestTunnelConnectRequest](../internal/relay/tunnel_test.go#L183)、[TestNewConnID](../internal/client/addr_test.go#L72) | ✅ 已实现 | **§11 自由度覆盖 §3.2 措辞**：ID 实为 client 生成（8B crypto/rand hex），relay 只按 key 组键并签发令牌；docs 已记录（[architecture.md:315-320](architecture.md#L315-L320)）。本行按 §11 判 ✅，README 说明缺失见 §11.1 |
| task.md §3.3 | relay 向 agent 控制连接下发接入通知（键+令牌），同时向 client 回复配对信息 | [internal/relay/tunnel.go:474-500](../internal/relay/tunnel.go#L474-L500)（MsgIncoming + MsgPairResult） | [TestTunnelConnectRequest](../internal/relay/tunnel_test.go#L183)、[TestE2ETCPPairingBothOrders](../test/e2e_basic_test.go#L104) | ✅ 已实现 | — |
| task.md §3.4 | agent 新建 TCP 连接首条消息带键+令牌；client 同样新建连接报到 | [internal/relay/tcp_data.go:106-157](../internal/relay/tcp_data.go#L106-L157)；[internal/agent/data.go:53-115](../internal/agent/data.go#L53-L115)；[internal/client/session_tcp.go:171-229](../internal/client/session_tcp.go#L171-L229) | [TestTCPPairAgentFirst](../internal/relay/tcp_data_test.go#L48)、[TestTCPPairClientFirst](../internal/relay/tcp_data_test.go#L72) | ✅ 已实现 | — |
| task.md §3.5 | **待配对表**处理两侧到达顺序不确定：先到者占位等待，另一侧到达且令牌有效即绑定；超时未凑齐则关闭已到连接并清理 | [internal/pairing/pairing.go:201-255](../internal/pairing/pairing.go#L201-L255)（Claim/ErrWaiting）；[internal/relay/tcp_data.go:202-259](../internal/relay/tcp_data.go#L202-L259)（:217-246 静默等待、:242-245 超时清理）；[internal/relay/relay.go:99-108](../internal/relay/relay.go#L99-L108)（OnExpire 关连接） | [TestClaimBothOrders](../internal/pairing/pairing_test.go#L73)、[TestTCPPairAgentFirst](../internal/relay/tcp_data_test.go#L48)、[TestTCPPairClientFirst](../internal/relay/tcp_data_test.go#L72)、[TestTCPPairTimeout](../internal/relay/tcp_data_test.go#L170)、[TestE2ETCPPairingBothOrders](../test/e2e_basic_test.go#L104) | ✅ 已实现 | 顶部另有 `pairWait` 引用计数（[:32-76](../internal/relay/tcp_data.go#L32-L76)）保证「先到者不写任何字节」 |
| task.md §3.6 | 绑定后双向 `io.Copy`，任一端关闭或超时即清理该配对键下全部资源 | [internal/relay/tcp_data.go:263-313](../internal/relay/tcp_data.go#L263-L313)（:272 Consume、:285-287 双向转发、:306 Remove）；[internal/forward/forward.go:83-145](../internal/forward/forward.go#L83-L145) | [TestE2ETCPEchoConcurrent20](../test/e2e_basic_test.go#L184)（结束后配对表回落 0）、[TestBidirectionalHalfClose](../internal/forward/forward_test.go#L105) | ✅ 已实现 | 异常/超时分别记 `data_close` / `idle_timeout`（[:291-304](../internal/relay/tcp_data.go#L291-L304)） |
| task.md §3.7 | **报到消息只在连接建立瞬间出现一次**，之后字节全部原样透传 | [internal/relay/tcp_data.go:131-139](../internal/relay/tcp_data.go#L131-L139)（只 ReadFrame 一次）→[:285-287](../internal/relay/tcp_data.go#L285-L287)（此后纯 Copy）；[docs/protocol.md:357-358](protocol.md#L357-L358) 不变量 6 | [TestTCPPairAgentFirst](../internal/relay/tcp_data_test.go#L48)（Bind 后连续 `echoRoundTrip` 三次）、[TestTCPPairClientFirst](../internal/relay/tcp_data_test.go#L72) | ✅ 已实现 | relay 配对成功前**零字节写入**（故意不发 BindOK），保证透传不被污染 |
| task.md §3.8 | UDP 双方各发报到数据报；relay 记录两侧 `(IP,Port)` 建立会话映射，后续按源地址反查配对键转发 | [internal/relay/udp_data.go:45-140](../internal/relay/udp_data.go#L45-L140)（:53 反查、:108 RegisterHalf、:122-131 回 BindOK 一次）；[internal/registry/udp_table.go:155-245](../internal/registry/udp_table.go#L155-L245)（双索引） | [TestNativeUDPForwardAndTimeout](../internal/relay/udp_data_test.go#L93)、[TestUDPTableHalfToFull](../internal/registry/registry_test.go#L328) | ✅ 已实现 | 半会话（对端未到）数据报丢弃，符合协议 |
| task.md §3.9 | UDP 需独立会话超时清理 | [internal/registry/udp_table.go:291-316](../internal/registry/udp_table.go#L291-L316)（Sweep）；[internal/relay/server.go:258-292](../internal/relay/server.go#L258-L292)（单 ticker 驱动） | [TestUDPTableSweep](../internal/registry/registry_test.go#L412)、[TestNativeUDPForwardAndTimeout](../internal/relay/udp_data_test.go#L93)、[TestE2EUDPModeNative](../test/e2e_udp_test.go#L18)（`800ms` 后表 1→0） | ✅ 已实现 | over-TCP 会话复用同一套 `udp_session_timeout`（[carrier.go:445](../internal/relay/carrier.go#L445)） |
| task.md §3.10 | 第一版不做源地址漂移；超时即清理并要求重连 | [internal/registry/udp_table.go:155-205](../internal/registry/udp_table.go#L155-L205)（端点冲突拒绝）；[docs/protocol.md:388-389](protocol.md#L388-L389) | [TestUDPTableConflicts](../internal/registry/registry_test.go#L386)、[TestE2EUDPModeNative](../test/e2e_udp_test.go#L18)（会话过期后旧 socket 发包被丢弃，换源端口重新配对） | ✅ 已实现 | 行为与 spec 一致（含 README §10 #4 声明） |
| task.md §3.11 | **TCP 与 UDP 必须共用同一套令牌校验机制** | [internal/token/token.go:97-135](../internal/token/token.go#L97-L135)（Validate/Consume，三处共用同一 Store）；调用点：[tcp_data.go:189](../internal/relay/tcp_data.go#L189)、[udp_data.go:98](../internal/relay/udp_data.go#L98)、[carrier.go:329-389](../internal/relay/carrier.go#L329-L389) | [TestIssueAndValidate](../internal/token/token_test.go#L39)、[TestConsume](../internal/token/token_test.go#L100)、[TestE2EFailureWrongToken](../test/e2e_failure_test.go#L13)、[TestE2EFailureTokenReuse](../test/e2e_failure_test.go#L37) | ✅ 已实现 | 令牌只存 SHA-256 + 常量时间比较（[:107](../internal/token/token.go#L107)） |

### 1.4 §4 ini 配置格式（7 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §4.1 | 统一 ini；三种模式都读 `[common]` 和自己段；服务映射用点号键扁平写法 | [internal/config/config.go:238-261](../internal/config/config.go#L238-L261)（Load 全段）、[:336-351](../internal/config/config.go#L336-L351)（load 顺序）；[internal/config/ini.go](../internal/config/ini.go) | [TestLoadValidRelay](../internal/config/config_test.go#L41)、[TestLoadValidAgent](../internal/config/config_test.go#L116)、[TestLoadValidClient](../internal/config/config_test.go#L156) | ✅ 已实现 | 未知段/键只 WARN（[:349](../internal/config/config.go#L349)） |
| task.md §4.2 | `[server] service.<服务ID> = <协议>:<本地端口>` 扁平点号键 | [internal/config/config.go:426-448](../internal/config/config.go#L426-L448)、[:562-580](../internal/config/config.go#L562-L580) | [TestLoadServiceDefaultConflict](../internal/config/config_test.go#L202)、[TestAddressNormalization](../internal/config/config_test.go#L341) | ✅ 已实现 | `service.default` 显式配置直接报错（[:436-438](../internal/config/config.go#L436-L438)） |
| task.md §4.3 | `[server]` 的 `protocol` + `local` 是主服务，**默认服务ID `default`** | [internal/config/config.go:35](../internal/config/config.go#L35)（`DefaultServiceID`）、[:157-168](../internal/config/config.go#L157-L168)（All 主服务在前）、[:422-424](../internal/config/config.go#L422-L424)（缺 protocol 按 tcp） | [TestLoadValidAgent](../internal/config/config_test.go#L116)、[TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)（relay 页含 `default`） | ✅ 已实现 | — |
| task.md §4.4 | `[server] service.<ID>` 协议取 tcp 或 udp；裸端口补 127.0.0.1 | [internal/config/config.go:564-580](../internal/config/config.go#L564-L580)、[:596-616](../internal/config/config.go#L596-L616) | [TestAddressNormalization](../internal/config/config_test.go#L341) | ✅ 已实现 | 仅允许 tcp/udp（[:570-573](../internal/config/config.go#L570-L573)） |
| task.md §4.5 | `[client] service.<ID> = <本地监听地址>`，为每个条目在本机开监听 | [internal/config/config.go:452-498](../internal/config/config.go#L452-L498)、[:491-495](../internal/config/config.go#L491-L495)（normalize）；[internal/client/listener.go:52-112](../internal/client/listener.go#L52-L112) | [TestLoadValidClient](../internal/config/config_test.go#L156)、[TestValidateDuplicateClientListen](../internal/config/config_test.go#L212) | ✅ 已实现 | 重复监听地址启动即失败（validate） |
| task.md §4.6 | `manage_listen` 三种模式都读，第一版只提供只读管理页 | [internal/relay/server.go:224-255](../internal/relay/server.go#L224-L255)；[internal/agent/adminpage.go:23-46](../internal/agent/adminpage.go#L23-L46)；[internal/client/admin.go:18-42](../internal/client/admin.go#L18-L42) | [TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11) | ✅ 已实现 | 空值=不启动；`0.0.0.0` 会 WARN（[validate.go:126-129](../internal/config/validate.go#L126-L129)） |
| task.md §4.7 | 上述语义 README 必须写清 | [README.md:341-375](../README.md#L341-L375) §5.6（主服务 default、附加服务、本地监听） | 文档核对（字段与 [config.go:80-202](../internal/config/config.go#L80-L202) 逐字段比对一致） | ✅ 已实现 | 11+10+10 个字段与 README §5.2–§5.5 表格一一对应，无遗漏 |

### 1.5 §5 管理页面（6 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §5.1 | 三种模式都在 `manage_listen` 提供 HTTP 服务与简单 HTML 管理页 | [internal/admin/admin.go:69-120](../internal/admin/admin.go#L69-L120)（框架）、[internal/admin/layout.go:78](../internal/admin/layout.go#L78)（1s 自动刷新）；三模式 `startAdmin` 见 §4.6 行 | [TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11)、[TestIndexAndPages](../internal/admin/admin_test.go#L79) | ✅ 已实现 | 无外部脚本资源（e2e 断言不含 `<script src=`） |
| task.md §5.2 | 第一版**只做查看**，不做配置写入；改配置走 ini + 重启 | 仅 relay 注册 1 个 `Action`（[server.go:234-239](../internal/relay/server.go#L234-L239)），agent/client 的 `admin.Options` 无 `Actions`（[adminpage.go:30-45](../internal/agent/adminpage.go#L30-L45)、[client/admin.go:23-30](../internal/client/admin.go#L23-L30)） | [TestE2EFailureKick](../test/e2e_failure_kick_idle_test.go#L19)（GET `/action/kick`→405、无 CSRF POST→403） | ✅ 已实现 | — |
| task.md §5.3 | relay 页面：服务注册表（ID、协议、在线状态、所属 agent）、在线 agent、当前配对表、UDP 会话表、踢出 | [internal/relay/admin.go:19-39](../internal/relay/admin.go#L19-L39)（概览）、[:43-62](../internal/relay/admin.go#L43-L62)（在线 agent）、[:65-90](../internal/relay/admin.go#L65-L90)（服务注册表，含「所属 agent」）、[:93-112](../internal/relay/admin.go#L93-L112)（配对表）、[:115-165](../internal/relay/admin.go#L115-L165)（两种 UDP 会话表）、[:167-296](../internal/relay/admin.go#L167-L296)（踢出 3 类） | [TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11)（:63 四块表头）、[TestAdminPageAndKick](../internal/relay/admin_test.go#L78)、[TestAdminNativeUDPSessionTable](../internal/relay/admin_test.go#L165) | ✅ 已实现 | 页首还显示「UDP 承载模式」（native/tcp_tunnel），task2 验收用到 |
| task.md §5.4 | server 页面：本机已注册服务列表、到 relay 的控制连接状态、当前活跃数据连接 | [internal/agent/adminpage.go:59-77](../internal/agent/adminpage.go#L59-L77)（本机已注册服务）、[:79-119](../internal/agent/adminpage.go#L79-L119)（控制连接）、[:121-143](../internal/agent/adminpage.go#L121-L143)（活跃数据连接） | [TestAdminPage](../internal/agent/data_test.go#L250)、[TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11)（:72 表头） | ✅ 已实现 | — |
| task.md §5.5 | client 页面：relay 返回的可用服务列表、本地端口映射表、当前活跃配对 | [internal/client/admin.go:70-94](../internal/client/admin.go#L70-L94)（可用服务列表）、[:96-114](../internal/client/admin.go#L96-L114)（本地端口映射表）、[:116-140](../internal/client/admin.go#L116-L140)（当前活跃配对）；数据源 [views.go:110-180](../internal/client/views.go#L110-L180) | [TestAdminPage](../internal/client/client_test.go#L286)、[TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11)（:81 表头） | ✅ 已实现 | — |
| task.md §5.6 | **踢出是唯一写操作**：关闭对应控制/配对连接句柄，让「关闭→转发退出→表项清理」既有路径统一处理，不另写清理逻辑 | [internal/relay/admin.go:167-191](../internal/relay/admin.go#L167-L191)（Action 入口）、[:193-296](../internal/relay/admin.go#L193-L296)（关句柄 + 发 MsgKicked）；框架 CSRF [internal/admin/admin.go:159-160](../internal/admin/admin.go#L159-L160) | [TestE2EFailureKick](../test/e2e_failure_kick_idle_test.go#L19)、[TestAdminKickUnknown](../internal/relay/admin_test.go#L152) | ✅ 已实现 | 踢出另有 3 个子路径（agent/pair/carrier），均只 Close + 走既有清理 |

### 1.6 §6 安全与加密（4 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §6.1 | 隧道端口信令使用 TLS；agent/client 用预置 CA 或证书校验 | relay：[internal/relay/server.go:177-211](../internal/relay/server.go#L177-L211)（`tls.Listen`，MinVersion 1.2）；agent：[internal/agent/util.go:30-56](../internal/agent/util.go#L30-L56)；client：[internal/client/control.go:170-208](../internal/client/control.go#L170-L208) | [TestTunnelTLSHelloOK](../internal/relay/tunnel_test.go#L247)、[TestE2ETLSTCPEcho](../test/e2e_tls_test.go#L106)、[TestE2ETLSInsecureSkipVerify](../test/e2e_tls_test.go#L157)、3 个「配置不匹配必须失败」用例（[:207](../test/e2e_tls_test.go#L207)/[:248](../test/e2e_tls_test.go#L248)/[:287](../test/e2e_tls_test.go#L287)） | ✅ 已实现 | TLS 为**部署时启用**（未配证书→明文+WARN，README §7.1 已声明）；"强制 TLS"属工程增强 E7，不算本行缺口 |
| task.md §6.2 | 通信端口保持哑管道，不终止、不解析上层协议 | [internal/relay/tcp_data.go:285-287](../internal/relay/tcp_data.go#L285-L287)；[docs/protocol.md:17-18](protocol.md#L17-L18) | [TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)（HTTP 头/体原样透传） | ✅ 已实现 | task2 允许的唯一例外：UDP over TCP 的**封装帧**（PairLink 自有格式），见 §T2.3 |
| task.md §6.3 | HTTPS/SSH 端到端加密由第三方与真实服务完成，中继透明转发 | [README.md:418-420](../README.md#L418-L420)；[docs/protocol.md:17-18](protocol.md#L17-L18)；转发层无任何加解密（[forward.go:115-131](../internal/forward/forward.go#L115-L131)） | [TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)（响应头 `X-PairLink-Test` 原样到达） | ✅ 已实现 | 数据面**永远明文**（见 README §10 #10） |
| task.md §6.4 | 一次性令牌与配对键绑定、短有效期、用后即焚，TCP/UDP 通信端口都校验 | [internal/token/token.go:75-152](../internal/token/token.go#L75-L152)（Issue/Validate/Consume/Sweep）；三处调用见 §3.11 行；默认 TTL 15s [config.go:216](../internal/config/config.go#L216) | [TestConsume](../internal/token/token_test.go#L100)、[TestExpiry](../internal/token/token_test.go#L117)、[TestE2EFailureTokenReuse](../test/e2e_failure_test.go#L37)、[TestE2EFailureTokenExpiry](../test/e2e_failure_test.go#L77)、[TestNativeUDPHalfPairTimeout](../internal/relay/udp_data_test.go#L171) | ✅ 已实现 | 日志不含令牌明文（[TestRelayLogsDoNotContainToken](../internal/relay/logs_test.go#L69)、[TestLogsDoNotContainToken](../internal/agent/tunnel_test.go#L237)） |

### 1.7 §7 技术约束（6 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §7.1 | 语言 Go，优先标准库；第三方库须说明理由 | [go.mod:1-5](../go.mod#L1-L5)（唯一依赖 `golang.org/x/sys`）；[README.md:13](../README.md#L13) 已说明理由（Windows SCM） | [TestCrossCompile](../test/binary_test.go#L492)（纯 Go 构建三平台） | ✅ 已实现 | — |
| task.md §7.2 | 单二进制、三子命令、统一 ini，各模式只读自己的段 | [main.go:89-104](../main.go#L89-L104)；[internal/config/config.go:336-351](../internal/config/config.go#L336-L351)；各 `Run` 只取对应段 | [TestSelectMode](../main_test.go#L11)、[TestLoadConfig](../main_test.go#L44) | ✅ 已实现 | 解析四段但只校验当前模式（README §5.1） |
| task.md §7.3 | 路径处理用 `filepath`；支持 Linux/Windows；README 说明两平台运行方式和防火墙 | [internal/config/config.go:547-560](../internal/config/config.go#L547-L560)（absPath）、[main.go:252-260](../main.go#L252-L260)（`filepath.Abs/FromSlash`）、[internal/service/service.go:73-79](../internal/service/service.go#L73-L79)；[README.md:427-495](../README.md#L427-L495) §8 | [TestSubprocessTCPEcho](../test/binary_test.go#L285)、Linux 真机 [README.md:928-945](../README.md#L928-L945) | ✅ 已实现 | 防火墙表 README §8.3 覆盖 TCP/UDP 与管理口 |
| task.md §7.4 | 双向转发用 `io.Copy` + `context`；任一端关闭另一方向及时释放；**无 goroutine 泄漏** | [internal/forward/forward.go:83-145](../internal/forward/forward.go#L83-L145)（2 copy + 1 看护，返回前全部 join）、[:178-207](../internal/forward/forward.go#L178-L207)（idle 包装）；relay 使用 [:285-287](../internal/relay/tcp_data.go#L285-L287) | [TestBidirectionalNoGoroutineLeak](../internal/forward/forward_test.go#L208)、[TestJoinContextNoLeak](../internal/forward/forward_test.go#L309)、[TestShutdownNoGoroutineLeak](../internal/agent/data_test.go#L220)、[TestGoroutineCleanup](../internal/client/client_test.go#L405)、[TestE2EGoroutineNoLeak](../test/e2e_goroutine_test.go#L17)（pprof `goroutine?debug=1` 无 `internal/forward.Bidirectional` 栈帧；退出后无 relay/agent/client/forward 栈帧） | ✅ 已实现 | pprof 证据在 [e2e_goroutine_test.go:81-127](../test/e2e_goroutine_test.go#L81-L127)；实测数字见 README §13.9（运行中 total=30，基线 2→2，容差 +2） |
| task.md §7.5 | 连接注册表需在 README 说明**数据结构与并发保护方式** | [README.md:71-86](../README.md#L71-L86)（5 个结构 + 锁 + 清理方式表，逐行对应 [architecture.md:145-252](architecture.md#L145-L252)） | 文档核对（结构名/键/锁类型与代码一致：`sync.RWMutex`/`sync.Mutex`） | ✅ 已实现 | 表里 `PairingTable` 的 ticker 周期与 [server.go:261-265](../internal/relay/server.go#L261-L265) 一致 |
| task.md §7.6 | 日志覆盖连接建立、注册、通知、配对、断开、超时清理、踢出等关键事件 | 生产代码 `"event"` 字面量共 **63** 个（清单见附录 A），关键：[tunnel.go:88](../internal/relay/tunnel.go#L88)、[tunnel.go:369](../internal/relay/tunnel.go#L369)、[tunnel.go:491](../internal/relay/tunnel.go#L491)、[tcp_data.go:279](../internal/relay/tcp_data.go#L279)、[tcp_data.go:307](../internal/relay/tcp_data.go#L307)、[tcp_data.go:292](../internal/relay/tcp_data.go#L292)、[admin.go:204](../internal/relay/admin.go#L204) | [TestRelayIdleTimeoutLogsEvent](../internal/relay/logs_test.go#L130)、[TestE2EFailureTokenReuse](../test/e2e_failure_test.go#L37)（日志断言）、[TestRelayLogsDoNotContainToken](../internal/relay/logs_test.go#L69) | ✅ 已实现 | 事件名稳定可检索，README §9 列了主要事件 |

### 1.8 §8 代码命名约定（7 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §8.1 | 配对键：`PairingKey` | [internal/pairing/pairing.go:36](../internal/pairing/pairing.go#L36) | [TestPairingKey](../internal/pairing/pairing_test.go#L37) | ✅ 已实现 | — |
| task.md §8.2 | 待配对表：`PairingTable` | [internal/pairing/pairing.go:155](../internal/pairing/pairing.go#L155) | [TestClaimBothOrders](../internal/pairing/pairing_test.go#L73) | ✅ 已实现 | — |
| task.md §8.3 | 服务注册表：`ServiceRegistry` | [internal/registry/service_registry.go:51](../internal/registry/service_registry.go#L51) | [TestServiceRegistryBasic](../internal/registry/registry_test.go#L55) | ✅ 已实现 | — |
| task.md §8.4 | 一次性令牌：`PairingToken` | [internal/token/token.go:51](../internal/token/token.go#L51)（`type PairingToken = Store` 别名） | [TestIssueAndValidate](../internal/token/token_test.go#L39) | ✅ 已实现 | 名称存在且被文档引用；实现主体是 `Store`（别名等价，不构成缺口） |
| task.md §8.5 | 三个监听器之一：`TunnelListener` | [internal/relay/tunnel.go:23](../internal/relay/tunnel.go#L23) | [TestTunnelHelloAuth](../internal/relay/tunnel_test.go#L16) | ✅ 已实现 | — |
| task.md §8.6 | 三个监听器之二：`TCPDataListener` | [internal/relay/tcp_data.go:22](../internal/relay/tcp_data.go#L22) | [TestTCPPairConcurrent](../internal/relay/tcp_data_test.go#L196) | ✅ 已实现 | 同一 listener 兼服纯透传与 over-TCP 承载（[handleBind:160-171](../internal/relay/tcp_data.go#L160-L171)） |
| task.md §8.7 | 三个监听器之三：`UDPDataListener` | [internal/relay/udp_data.go:20](../internal/relay/udp_data.go#L20) | [TestNativeUDPForwardAndTimeout](../internal/relay/udp_data_test.go#L93) | ✅ 已实现 | 仅 `udp_mode=native` 时创建（[server.go:110-124](../internal/relay/server.go#L110-L124)） |

### 1.9 §9 交付物（7 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §9.1 | 完整可编译 Go 项目，module `github.com/lsong98sh/pairlink`，单一二进制，Linux/Windows 可编译运行 | [go.mod:1](../go.mod#L1)；[main.go:31](../main.go#L31)（版本单一来源） | [TestCrossCompile](../test/binary_test.go#L492)、[TestSubprocessTCPEcho](../test/binary_test.go#L285)、Linux 三进程 [README.md:928-945](../README.md#L928-L945) | ✅ 已实现 | 三平台产物大小见 README §13.2 第 13 行 |
| task.md §9.2 | README 六项：架构、协议消息定义与状态机图、ini 全部字段（含点号键与 default）、三模式部署运行、管理页说明、第一句定位 | [README.md:3](../README.md#L3)、[:31-57](../README.md#L31-L57)（架构）、[:88-145](../README.md#L88-L145)（协议表+状态机）、[:272-386](../README.md#L272-L386)（ini 全字段）、[:389-403](../README.md#L389-L403)（管理页）、[:427-495](../README.md#L427-L495)（部署） | 文档核对（另见 §3 交付物展开表） | ✅ 已实现 | 字段与 `Config` 结构体逐项比对无遗漏 |
| task.md §9.3.1 | e2e：本地 HTTP 零改造，经 agent 注册、client 本地端口访问成功 | 同上 §2.14 | [TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)（HTTP 200 + 自定义头原样） | ✅ 已实现 | — |
| task.md §9.3.2 | e2e：TCP echo 或 RDP 走通注册→通知→回连→配对→转发→断开清理 | 同上 §3.4/§3.6 | [TestE2ETCPPairingBothOrders](../test/e2e_basic_test.go#L104)、[TestE2ETCPEchoConcurrent20](../test/e2e_basic_test.go#L184) | ✅ 已实现 | 两种报到顺序各跑一遍 |
| task.md §9.3.3 | e2e：UDP 走通报到、会话映射、转发、超时清理 | 同上 §3.8/§3.9 | [TestE2EUDPModeNative](../test/e2e_udp_test.go#L18)（会话表 1→0、旧 socket 丢弃、新源端口重配） | ✅ 已实现 | over-TCP 见 §T2.6 |
| task.md §9.3.4 | e2e：同一 agent 同时注册多个服务；并发多组配对互不干扰 | 同上 §2.8 | [TestE2EAgentMultiServiceConcurrent](../test/e2e_basic_test.go#L278)（3 服务 × 5 轮并发，无串流） | ✅ 已实现 | — |
| task.md §9.3.5 | e2e：令牌错误/复用/过期、控制连接断开、踢出、空闲超时清理的验证结果 | 同上 §6.4/§5.6 | [TestE2EFailureWrongToken](../test/e2e_failure_test.go#L13)、[TestE2EFailureTokenReuse](../test/e2e_failure_test.go#L37)、[TestE2EFailureTokenExpiry](../test/e2e_failure_test.go#L77)、[TestE2EFailureControlDrop](../test/e2e_failure_test.go#L145)、[TestE2EFailureKick](../test/e2e_failure_kick_idle_test.go#L19)、[TestE2EFailureIdleTimeout](../test/e2e_failure_kick_idle_test.go#L147) | ✅ 已实现 | 六条异常路径各有独立 PASS 记录（README §13.7） |

### 1.10 §10 验收标准（6 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §10.1 | 本地第三方服务零改造即可被远端访问 | 见 §2.14 | [TestE2EHTTPZeroChange](../test/e2e_basic_test.go#L18)、[TestSubprocessTCPEcho](../test/binary_test.go#L285)（真实二进制） | ✅ 已实现 | — |
| task.md §10.2 | TCP 与 UDP 各一个通信端口上支持多连接、多服务、多协议并发复用，靠配对键区分 | 配对键索引：[pairing.go:36-82](../internal/pairing/pairing.go#L36-L82)、[udp_table.go:120-244](../internal/registry/udp_table.go#L120-L244)、[carrier.go:329-464](../internal/relay/carrier.go#L329-L464) | [TestE2ETCPEchoConcurrent20](../test/e2e_basic_test.go#L184)、[TestE2EAgentMultiServiceConcurrent](../test/e2e_basic_test.go#L278)、[TestE2EUDPModeTCPTunnel](../test/e2e_udp_test.go#L117)（2 会话复 1 条承载） | ✅ 已实现 | — |
| task.md §10.3 | 令牌异常均拒绝绑定；agent 掉线、踢出、超时后无僵尸配对和 goroutine 泄漏（pprof 或计数佐证） | 清理路径见 §3.6/§3.9；关停 [server.go:536-571](../internal/relay/server.go#L536-L571) | [TestE2EGoroutineNoLeak](../test/e2e_goroutine_test.go#L17)、[TestE2EFailureControlDrop](../test/e2e_failure_test.go#L145)、[TestAgentGoneCleansPendingPair](../internal/relay/tcp_data_test.go#L270)、[TestAlreadyPairedRetentionWindow](../internal/relay/retention_test.go#L23) | ✅ 已实现 | pprof `total` 计数 + 栈帧双重断言；容忍 +2 goroutine（测试框架自身） |
| task.md §10.4 | 三种模式的 `manage_listen` 页面均可访问并正确展示各自状态 | 见 §1.5 | [TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11)（三端口 200、标题、表头、pprof） | ✅ 已实现 | — |
| task.md §10.5 | 单二进制在 Linux 与 Windows 上均能按三种子命令正常启动和运行 | [main.go:145-185](../main.go#L145-L185)（signal + 三模式） | [TestSubprocessTCPEcho](../test/binary_test.go#L285)、[TestSubprocessGracefulShutdown](../test/binary_test.go#L371)、[TestBinaryVersionUsageAndConfigErrors](../test/binary_test.go#L82)；Linux 真机 [README.md:928-945](../README.md#L928-L945)、跨 OS [README.md:974-1001](../README.md#L974-L1001) | ✅ 已实现 | — |
| task.md §10.6 | Windows 提供服务注册/取消/启动/停止：`-service install/start/stop/uninstall`，服务名 `pairlink-<mode>` | [main.go:236-307](../main.go#L236-L307)（四个子命令）、[internal/service/service.go:66-79](../internal/service/service.go#L66-L79)（命名/日志路径）；[internal/service/service_windows.go:31](../internal/service/service_windows.go#L31)/[:71](../internal/service/service_windows.go#L71)/[:99](../internal/service/service_windows.go#L99)/[:122](../internal/service/service_windows.go#L122)/[:149](../internal/service/service_windows.go#L149)/[:173](../internal/service/service_windows.go#L173) | [TestE2EWindowsService](../test/e2e_service_windows_test.go#L21)（**仅非管理员分支**：退出码 1 + 可读错误 + 命名）；[TestManagerAPIShape](../internal/service/service_windows_test.go#L24)、[TestQueryMissingService](../internal/service/service_windows_test.go#L46)、[TestName](../internal/service/service_test.go#L8)、[TestLogPath](../internal/service/service_test.go#L36) | 🔒 环境受限 | **代码与单测齐全，但 install→start→stop→uninstall 的真实 SCM 状态与 `pairlink-<mode>.log` 落盘未在本机执行**（本机非管理员）。需管理员环境人工复核，见 §5 G3 |
| **小计** | | | | **5 ✅ / 1 🔒** | |

### 1.11 §11 实现自由度（5 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §11.1 | README 说明**客户端连接ID 的生成方** | 实现：[internal/client/client.go:383-390](../internal/client/client.go#L383-L390)（client 生成 8B hex）；文档：[docs/architecture.md:315-320](architecture.md#L315-L320)、[docs/protocol.md:190](protocol.md#L190) | [TestNewConnID](../internal/client/addr_test.go#L72)（长度/字符集/唯一性） | 🟡 部分实现 | **README 全文未出现「client_conn_id 由 client 生成」**（仅出现在 `docs/`）；task.md §11 明确要求"在 README 说明"。补 1 行即可闭环（§5 G1） |
| task.md §11.2 | README 说明**管理页 HTML 组织方式** | 实现：统一布局/内联 CSS/无外部资源 [internal/admin/layout.go:1-140](../internal/admin/layout.go#L1-L140)、各模式只写 `<section>`（[relay/admin.go:17](../internal/relay/admin.go#L17)、[agent/adminpage.go:50-57](../internal/agent/adminpage.go#L50-L57)、[client/admin.go:47](../internal/client/admin.go#L47)）；文档：[docs/architecture.md:326-330](architecture.md#L326-L330) | [TestE2EAdminPagesThreeModes](../test/e2e_admin_test.go#L11)（断言 `html lang="zh-CN"`、无 `<script src=`） | 🟡 部分实现 | README §6 只描述**页面内容**，未说明 HTML 如何组织（统一布局/内联样式/各模式只写 section）。§5 G2 |
| task.md §11.3 | README 说明**令牌有效期策略** | [README.md:416-417](../README.md#L416-L417)（默认 15s、用后即焚、只存 SHA-256、常量时间比较）；[README.md:309](../README.md#L309)（`token_ttl`）；[config.go:216](../internal/config/config.go#L216) | [TestDefaultTTL](../internal/token/token_test.go#L173)、[TestExpiry](../internal/token/token_test.go#L117)、[TestE2EFailureTokenExpiry](../test/e2e_failure_test.go#L77) | ✅ 已实现 | 策略已说明（短 TTL + Consume + 摘要存储） |
| task.md §11.4 | README 说明 **UDP 会话超时阈值** | [README.md:308](../README.md#L308)（relay 60s）、[:324](../README.md#L324)（agent 60s）、[:339](../README.md#L339)（client 60s）；[config.go:215](../internal/config/config.go#L215)/[:221](../internal/config/config.go#L221)/[:228](../internal/config/config.go#L228) | [TestClientUDPSessionTimeout](../internal/config/client_timeout_test.go#L13)、[TestNativeUDP](../internal/client/udp_test.go#L81)（300ms 清理生效） | ✅ 已实现 | 三处默认值均已写明；**选择理由**未写（P3 可选补充，不算缺口） |
| task.md §11.5 | README 说明**日志格式** | [README.md:534-541](../README.md#L534-L541)（slog、text/json、字段、事件、Windows 落盘）；[internal/logx/logx.go](../internal/logx/logx.go) | [TestNewText](../internal/logx/logx_test.go#L13)、[TestNewJSON](../internal/logx/logx_test.go#L29)、[TestNewFileLogger](../internal/logx/logx_test.go#L78) | ✅ 已实现 | — |
| **小计** | | | | **3 ✅ / 2 🟡** | |

### 1.12 §12 版本与许可证（2 条）

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task.md §12.1 | 第一版打 `v0.1.0` 标签 | [internal/version/version.go:12](../internal/version/version.go#L12)（v0.2.0 为 `Version = "v0.2.0"`；v0.1.0 时点为 `Version = "v0.1.0"`）；`git tag -l` 实测有 `v0.1.0` 与 `v0.2.0` | [TestVersionConstant](../main_test.go#L79)、[TestVersionFormat](../internal/version/version_test.go#L9)、[TestBinaryVersionUsageAndConfigErrors](../test/binary_test.go#L84)（`pairlink v0.2.0`） | ✅ 已实现 | 版本号单一来源，三模式管理页共用 |
| task.md §12.2 | 许可证选 Apache-2.0 | [LICENSE:1-3](../LICENSE#L1-L3)（Apache License 2.0 正文）；[README.md:11](../README.md#L11) | 文档核对（无自动化测试，属交付物） | ✅ 已实现 | — |

---

## 2. task2.txt 逐条

| 需求ID | 需求原文摘要 | 实现位置 | 测试证据 | 状态 | 差距说明 |
| --- | --- | --- | --- | --- | --- |
| task2 L1 | `[relay] udp_listen` 改为**可选**：存在则启用原生 UDP 数据面；缺失/为空则不监听 UDP 端口，UDP 自动走 TCP 通信端口上的封装隧道 | [internal/config/config.go:98-101](../internal/config/config.go#L98-L101)（字段注释）、[config.go:372-376](../internal/config/config.go#L372-L376)（loadRelay）；[internal/config/validate.go:137-162](../internal/config/validate.go#L137-L162)（可选 + WARN）；[internal/relay/relay.go:87-93](../internal/relay/relay.go#L87-L93)（启动时派生 `udp_mode`）；[internal/relay/server.go:110-124](../internal/relay/server.go#L110-L124)（native 才 listen） | [TestRelayUDPListenOptional](../internal/config/config_test.go#L239)、[TestValidateErrors](../internal/config/config_test.go#L461)、[TestE2EUDPModeTCPTunnel](../test/e2e_udp_test.go#L117) | ✅ 已实现 | 未配置时 validate 追加一条 WARN 提示走 tcp_tunnel（不报错） |
| task2 L2 | relay 在隧道端口响应中携带 `udp_mode`（native/tcp_tunnel），**agent 注册响应**与 **client 服务列表响应**都要带 | [internal/relay/tunnel.go:210](../internal/relay/tunnel.go#L210)（HelloOK）、[:362-366](../internal/relay/tunnel.go#L362-L366)（ServiceRegistered）、[:385-390](../internal/relay/tunnel.go#L385-L390)（ListServices）、[:616-624](../internal/relay/tunnel.go#L616-L624)（订阅推送的全量 ServiceList）；字段定义 [internal/protocol/messages.go:63](../internal/protocol/messages.go#L63)、[:91](../internal/protocol/messages.go#L91)、[:117](../internal/protocol/messages.go#L117) | [TestE2EUDPModeNegotiation](../test/e2e_udp_test.go#L221)（native 与 tcp_tunnel 两种模式下三处字段一致性 + 端口字段）、[TestMissingUDPModeWarnsAndFallsBack](../internal/agent/agent_test.go#L154) | ✅ 已实现 | 消费侧：agent [control.go:201-231](../internal/agent/control.go#L201-L231) 记录/回退，client [control.go:435-452](../internal/client/control.go#L435-L452) 解析 |
| task2 L3 | TCP 通信端口报到消息增加 `carrier`（tcp / udp-over-tcp），relay 据此切换纯透传或帧解析；**帧头属 PairLink 封装，不违反协议无关** | 字段：[internal/protocol/messages.go:183-185](../internal/protocol/messages.go#L183-L185)；分流：[internal/relay/tcp_data.go:160-171](../internal/relay/tcp_data.go#L160-L171)（`:162` 纯透传、`:164` 转帧解析）；常量 [messages.go:24-27](../internal/protocol/messages.go#L24-L27)；原则说明 [docs/protocol.md:88-89](protocol.md#L88-L89) | [TestCarrierFirstFrameAuthFailure](../internal/relay/carrier_test.go#L90)、[TestTCPPairUnknownCarrier](../internal/relay/tcp_data_test.go#L258)、[TestCarrierMultiplexAndCleanup](../internal/relay/carrier_test.go#L115) | ✅ 已实现 | 一条连接只有一个模式，不发中途切换（[protocol.md:431-432](protocol.md#L431-L432)） |
| task2 L4 | UDP 隧道帧格式：长度前缀 + 会话标识 + 载荷，支持**一条 TCP 连接复用多个 UDP 会话** | 编解码：[internal/protocol/protocol.go:296-342](../internal/protocol/protocol.go#L296-L342)、[:322](../internal/protocol/protocol.go#L322)；上限常量 [:35-39](../internal/protocol/protocol.go#L35-L39)（`MaxSessionIDLen=129`、`MaxUDPTunnelFrameLen=65665`）；relay 会话表/转发：[internal/relay/carrier.go:23-66](../internal/relay/carrier.go#L23-L66)、[:125-273](../internal/relay/carrier.go#L125-L273)；agent 复用：[internal/agent/tunnel.go:56-152](../internal/agent/tunnel.go#L56-L152)；client 复用：[internal/client/tunnel.go:146-200](../internal/client/tunnel.go#L146-L200) | [TestUDPTunnelFrameRoundTrip](../internal/protocol/protocol_test.go#L285)、[TestUDPTunnelFrameLimits](../internal/protocol/protocol_test.go#L308)、[TestCarrierMultiplexAndCleanup](../internal/relay/carrier_test.go#L115)、[TestE2EUDPModeTCPTunnel](../test/e2e_udp_test.go#L117)（2 会话只有 2 个承载对端地址，见 [e2e_udp_test.go:207](../test/e2e_udp_test.go#L207)） | ✅ 已实现 | 单帧写用一次 `Write` + 发送侧 `writeMu`，避免帧交错 |
| task2 L5 | **README 写明取舍**：UDP over TCP 省一个公网端口，但引入队头阻塞，实时性敏感场景应配置原生 `udp_listen` | [README.md:147-160](../README.md#L147-L160) §3.3（两种模式对照表 + 显式「取舍」引用块）、[:516-517](../README.md#L516-L517) §10 #2、[:485](../README.md#L485) 防火墙表；协议侧 [docs/protocol.md:443-444](protocol.md#L443-L444) | 文档核对（README 有"省一个公网端口/队头阻塞/实时性敏感应配原生"三要素） | ✅ 已实现 | 与 task2 原文措辞一一对应 |
| task2 L6 | 验收：不配 `udp_listen` 时 UDP 能经 TCP 隧道通信；配了走原生 UDP；两种模式下 agent/client 都按 `udp_mode` 正确选择承载 | 选择逻辑：client [control.go:435-452](../internal/client/control.go#L435-L452)、[tunnel.go:291-330](../internal/client/tunnel.go#L291-L330)（over-TCP）/[session_udp.go:204-269](../internal/client/session_udp.go#L204-L269)（native）；agent [control.go:322-340](../internal/agent/control.go#L322-L340)、[tunnel.go:415-452](../internal/agent/tunnel.go#L415-L452)（over-TCP）/[data.go:116-189](../internal/agent/data.go#L116-L189)（native） | [TestE2EUDPModeTCPTunnel](../test/e2e_udp_test.go#L117)、[TestE2EUDPModeNative](../test/e2e_udp_test.go#L18)、[TestE2EUDPModeNegotiation](../test/e2e_udp_test.go#L221)、[TestUDPTunnel](../internal/client/udp_test.go#L179)、[TestTCPTunnelTwoSessionsOnOneCarrier](../internal/agent/tunnel_test.go#L15) | ✅ 已实现 | 两种承载端到端都通，且管理页分别显示 `native` / `tcp_tunnel` 的实际形态 |
| **小计** | | | | **6 ✅** | |

---

## 3. 交付物与验收标准证据展开（task.md §9 / §10 / §12）

> 本节**不新增条目**，只是把 §1.9–§1.12 已计分的行展开成可逐项核对的证据清单。

| 交付要求 | 现状证据 | 结论 |
| --- | --- | --- |
| README 内容（架构/协议/ini/部署/管理页/首句） | 六项在 [README.md:3](../README.md#L3)、[:31-57](../README.md#L31-L57)、[:88-145](../README.md#L88-L145)、[:272-386](../README.md#L272-L386)、[:389-403](../README.md#L389-L403)、[:427-495](../README.md#L427-L495)；`Config` 结构体字段 100% 覆盖 | ✅ 满足 |
| e2e 覆盖（HTTP/TCP/UDP/多服务/异常） | `test` 包 29 个顶层用例（Windows，含 1 个 `//go:build windows`）/28 个（Linux）；五类验收项各有独立用例，见 §1.9 五行 | ✅ 满足 |
| 双平台 | 交叉编译 3 目标通过（[TestCrossCompile](../test/binary_test.go#L492)）；Windows 真实三子进程（[TestSubprocessTCPEcho](../test/binary_test.go#L285)）；WSL2 Ubuntu 真实 Linux 三进程 116 条断言（[README.md:928-972](../README.md#L928-L972)）+ 跨 OS 联调 13 条（[:974-1001](../README.md#L974-L1001)） | ✅ 满足 |
| Windows 服务（4 子命令 + `pairlink-<mode>`） | 代码与命名单测齐全；真实 SCM 生命周期未执行（非管理员） | 🔒 环境受限（§5 G3） |
| `v0.1.0` / `v0.2.0` 标签 | `git tag -l` → `v0.1.0`、`v0.2.0`；`-version` 输出 `pairlink v0.2.0` | ✅ 满足 |
| Apache-2.0 | `LICENSE` 正文 + README §14 | ✅ 满足 |

---

## 4. 命名与自由度约定核对（task.md §8 / §11）

### 4.1 §8 七个必须类型名

`grep -rn "^type (PairingKey|PairingTable|ServiceRegistry|PairingToken|TunnelListener|TCPDataListener|UDPDataListener)\b" internal/` 实测 7/7 命中：

| 约定名 | 实际声明 | 位置 | 结论 |
| --- | --- | --- | --- |
| `PairingKey` | `type PairingKey struct` | [internal/pairing/pairing.go:36](../internal/pairing/pairing.go#L36) | ✅ 完全一致 |
| `PairingTable` | `type PairingTable struct` | [internal/pairing/pairing.go:155](../internal/pairing/pairing.go#L155) | ✅ 完全一致 |
| `ServiceRegistry` | `type ServiceRegistry struct` | [internal/registry/service_registry.go:51](../internal/registry/service_registry.go#L51) | ✅ 完全一致 |
| `PairingToken` | `type PairingToken = Store`（别名） | [internal/token/token.go:51](../internal/token/token.go#L51) | ✅ 名称存在；实现在 `Store` 上，`PairingToken` 可用（别名） |
| `TunnelListener` | `type TunnelListener struct` | [internal/relay/tunnel.go:23](../internal/relay/tunnel.go#L23) | ✅ 完全一致 |
| `TCPDataListener` | `type TCPDataListener struct` | [internal/relay/tcp_data.go:22](../internal/relay/tcp_data.go#L22) | ✅ 完全一致 |
| `UDPDataListener` | `type UDPDataListener struct` | [internal/relay/udp_data.go:20](../internal/relay/udp_data.go#L20) | ✅ 完全一致 |

### 4.2 §11 五个自由度在 README 的说明情况

| # | 自由度 | README 是否说明 | 具体位置 / 缺口 |
| --- | --- | --- | --- |
| 1 | 客户端连接ID 的生成方 | **否（🟡）** | 实现在 [client.go:396](../internal/client/client.go#L396)，说明只在 [architecture.md:315-320](architecture.md#L315-L320) 与 [protocol.md:190](protocol.md#L190)；README 零提及 → G1 |
| 2 | 管理页 HTML 组织方式 | **否（🟡）** | README §6 只讲页面内容/刷新/CSRF/无鉴权；布局与「只写 `<section>`」的组织方式只在 [architecture.md:326-330](architecture.md#L326-L330) 与代码 → G2 |
| 3 | 令牌有效期策略 | 是 | [README.md:317](../README.md#L317)（默认 15s 可配）+ [:437](../README.md#L437)（短 TTL、用后即焚、SHA-256、常量时间比较） |
| 4 | UDP 会话超时阈值 | 是（默认值） | [README.md:316](../README.md#L316)/[:332](../README.md#L332)/[:349](../README.md#L349)；未写"为什么是 60s"（P3 可选） |
| 5 | 日志格式 | 是 | [README.md:534-541](../README.md#L534-L541)（slog、text/json、统一字段、事件清单、Windows 落盘） |

---

## 5. 差距清单与优先级

> **口径**：`部分实现`/`仅环境受限` = 与 task.md 字面要求存在差距，计入需求侧；
> `工程增强` = task.md/task2 未要求、属后续可用性与可运维性投入，**不计入需求缺口**。

### 5.1 需求侧差距（3 项）

| ID | 差距 | 类型 | 影响 | 优先级 | 建议归属阶段 |
| --- | --- | --- | --- | --- | --- |
| G1 | README 未说明 `client_conn_id` 由 client 生成（§11.1） | 部分实现（文档） | §11 字面要求未闭环；联调/审计需翻 `docs/` 才能确认 ID 归属 | **P2** | 阶段 B（部署运维件，纯文档 1 段） |
| G2 | README 未说明管理页 HTML 组织方式（§11.2） | 部分实现（文档） | 同上；后续改管理页时无 README 约定可依 | **P3** | 阶段 B（纯文档 1 段） |
| G3 | Windows 服务完整生命周期未验证：`install → start → stop → uninstall` 的 SCM 状态迁移、`pairlink-<mode>.log` 落盘 | 🔒 仅环境受限 | §10.6 只能证明"代码在、错误路径正确"；交付验收存在"未证明"项 | **P1**（需管理员环境） | 阶段 B 收尾 + 人工管理员复核（非代码改动） |

### 5.2 工程增强（task.md / task2 未要求，不计入需求缺失）

> **阶段 6E 复核**：v0.1.0 时点列出的 E1–E6 已在阶段 6C/6D 全部实现，下表状态改为「✅ 已实现」
> 并补充 v0.2.0 实现位置与测试证据；E7 仍未做，E8（文档时点）已在本轮标注/修正。

| ID | 增强项 | 状态 | 实现位置（v0.2.0） | 测试证据 | 备注 |
| --- | --- | --- | --- | --- | --- |
| E1 | agent/client 控制连接断开后**自动重连**（指数退避+抖动+上限；重连后重新注册/重拉列表/重建本地监听/在途会话处置） | ✅ 已实现 | [internal/reconnect/reconnect.go:34](../internal/reconnect/reconnect.go#L34)（`Backoff`/`Delay`/`Wait`）；[internal/agent/reconnect.go:73](../internal/agent/reconnect.go#L73)、[internal/client/reconnect.go:73](../internal/client/reconnect.go#L73)（`supervise`）；[config.go:152-158](../internal/config/config.go#L152-L158)（`[server]` 字段）、[:209-214](../internal/config/config.go#L209-L214)（`[client]`） | [TestReconnectBackoffCap](../internal/reconnect/reconnect_test.go#L11)、[TestSleepCancelable](../internal/reconnect/reconnect_test.go#L93)、[TestReconnectAfterControlDrop（agent）](../internal/agent/reconnect_test.go#L52)、[TestReconnectAfterControlDrop（client）](../internal/client/reconnect_test.go#L30)、[TestE2EReconnectAfterControlCut](../test/reconnect_e2e_test.go#L135) | 默认 `reconnect=true`、`reconnect_max_backoff=60s`；`false` 回退 v0.1.0 行为；被踢出/relay 停机不重连 |
| E2 | 订阅收到 `proto` 变化时**热切换**本地监听（TCP↔UDP） | ✅ 已实现 | [internal/client/listener.go:232](../internal/client/listener.go#L232)（`hotSwitchProto`）、[:296](../internal/client/listener.go#L296)（`reconcileListeners`）；[views.go:115](../internal/client/views.go#L115)（事件入口） | [TestHotSwitchTCPToUDP](../internal/client/reconnect_test.go#L197)、[TestHotSwitchUDPToTCP](../internal/client/reconnect_test.go#L252)、[TestHotSwitchRollbackOnFailure](../internal/client/reconnect_test.go#L298)、[TestHotSwitchRepeatedNoLeak](../internal/client/reconnect_test.go#L379) | 原地址重开；失败回滚旧协议；已建会话继续自然结束、不迁移 |
| E3 | UDP over TCP **每会话发送队列/写者 goroutine**，消除队头阻塞对读循环的放大 | ✅ 已实现 | [internal/tunnelqueue/queue.go:64](../internal/tunnelqueue/queue.go#L64)（`Queue`）、[:145](../internal/tunnelqueue/queue.go#L145)（`SendData`）、[:249](../internal/tunnelqueue/queue.go#L249)（`writeLoop`）；接入 [agent/tunnel.go:176](../internal/agent/tunnel.go#L176)、[client/tunnel.go:60](../internal/client/tunnel.go#L60)、[relay/carrier.go:291](../internal/relay/carrier.go#L291) | [TestTunnelQueueDropPolicy](../internal/tunnelqueue/queue_test.go#L101)、[TestTunnelQueueCloseDrains](../internal/tunnelqueue/queue_test.go#L183)、[TestTunnelQueueEnqueueNeverBlocks](../internal/tunnelqueue/queue_test.go#L269)、[TestTunnelQueueNoGoroutineLeak](../internal/tunnelqueue/queue_test.go#L297)、[TestCarrierSlowSessionDoesNotBlockOthers](../internal/relay/carrier_slow_test.go#L21) | 字节+条数双限；满队列丢弃并记 `tunnel_drop`；关闭排空 |
| E4 | 原生 UDP 报到/BindOK 丢包处理（首包重传、幂等） | ✅ 已实现 | [internal/agent/data.go:113-117](../internal/agent/data.go#L113-L117)（约 300ms × 3）、[:198](../internal/agent/data.go#L198)（重发循环）；[internal/client/session_udp.go:174-178](../internal/client/session_udp.go#L174-L178)；[internal/relay/udp_data.go:52](../internal/relay/udp_data.go#L52)（半开重复 Bind 幂等）、[:143](../internal/relay/udp_data.go#L143)（`Full` 后丢弃） | [TestNativeUDPBindOKLostRetry（agent）](../internal/agent/reconnect_test.go#L187)、[TestNativeUDPBindOKLostRetry（client）](../internal/client/reconnect_test.go#L423)、[TestNativeUDPBindIdempotent](../internal/relay/udp_bind_test.go#L29)、[TestNativeUDPFullDuplicateBindDropped](../internal/relay/udp_bind_test.go#L65) | 收到 `BindOK` 或首个业务数据报即就绪；`bind_dup_drop` |
| E5 | soak/压力测试（长时间稳定、资源增长曲线） | ✅ 已实现 | [test/soak_test.go:49](../test/soak_test.go#L49)（`TestSoak`）、[:54](../test/soak_test.go#L54)（`TestSoakShort`）、[test/soak_metrics_windows.go:1](../test/soak_metrics_windows.go#L1)（跨平台资源采样）；[README.md:1150](../README.md#L1150) §13.17 | `go test -tags soak ./test -run TestSoakShort -timeout 10m`（30 轮 / 2m30s 全绿，TCP/UDP 每轮 4/4）；完整长跑命令见 README §13.17 | 默认 30 轮 / 150s；可配 `SOAK_ROUNDS`/`SOAK_DURATION`/`SOAK_SESSIONS` |
| E6 | systemd unit（三模式）、安装/卸载 shell、Windows PowerShell 安装/卸载、Dockerfile、docker-compose、`examples/*.ini` | ✅ 已实现 | [deploy/systemd/pairlink-relay.service:1](../deploy/systemd/pairlink-relay.service#L1)、[deploy/linux/install.sh:1](../deploy/linux/install.sh#L1)、[deploy/windows/install-service.ps1:1](../deploy/windows/install-service.ps1#L1)、[Dockerfile:1](../Dockerfile#L1)、[deploy/docker-compose.yml:1](../deploy/docker-compose.yml#L1)、[examples/relay.ini:1](../examples/relay.ini#L1) | [TestExamples](../test/examples_test.go#L46)（8 份模板全部 Load/Validate）；[README.md:1135](../README.md#L1135) §13.16 WSL2 systemd 实测 | Docker 构建与 Windows 管理员 SCM 生命周期仍未验证（环境限制） |
| E7 | TLS 强制开关（`tls_required` 或 relay 侧拒绝明文） | ❌ 未做 | 现状：[internal/relay/server.go:198](../internal/relay/server.go#L198)（未配证书即明文 + WARN） | 文档兜底：[README.md:429](../README.md#L429) §7.1 | 非需求缺口；P3，保留为后续增强 |
| E8 | README 阶段 4 时点数据/命令过期（`33 个 PASS`、`taskkill /PID`） | ✅ 已实现（文档） | [README.md:705](../README.md#L705)（§13.1 已标注「阶段 4 收尾」）、[README.md:862](../README.md#L862)（§13.12 同时点标注）；[README.md:841](../README.md#L841)（`taskkill` 已改为 `os.Process.Kill()`） | 与 §13.15.6 的 27 个 `--- PASS` 并存但已标明时点，不再自相矛盾 | 纯文档修正 |

---

## 6. 文档与实现不一致之处（阶段 6E 复核）

> D1/D2/D5 已在阶段 6E 关闭；D3/D4/D6 仍为描述性漂移，不影响语义。

| # | 位置 | 现象 | 事实 | 影响 |
| --- | --- | --- | --- | --- |
| D1 | [README.md:841](../README.md#L841)（§13.10 `TestSubprocessTCPEcho` 条目） | 曾写"最后 `taskkill /PID` 清理" | **阶段 6E 已修**：改为跨平台 `os.Process.Kill()`，与 [test/binary_test.go](../test/binary_test.go) 现状一致 | 已关闭 |
| D2 | [README.md:705](../README.md#L705)（§13.1 实测）、[:862](../README.md#L862)（§13.12） | 写"33 个 `--- PASS`" | **阶段 6E 已标注时点**：两处均写明「阶段 4 收尾」；当前 `test` 包顶层用例为 **29** 个（Windows，含 1 个 windows-only）/28 个（Linux），见 §13.15.6 | 已关闭（保留历史快照） |
| D3 | [docs/architecture.md:230-252](architecture.md#L230-L252)（§4.6 carrier 表） | 草图字段为 `agentCarrier/clientCarrier`、`carrier{conn,writeMu}` | 实际实现为 `carrierSession{key,agent,client,full,agentID,createdAt,lastSeen}` + `carrierConn{...,writeMu,closeOnce,closed}`（[carrier.go:28-61](../internal/relay/carrier.go#L28-L61)） | 仅示意性漂移，不影响语义；写作"建议结构"即可 |
| D4 | [docs/architecture.md:98-99](architecture.md#L98-L99)（§3.1 末条） | 写踢出"只调用 `Control.Close()` 或数据连接 `Close()`" | 实际踢出还会先写 `MsgKicked`（[relay/admin.go:212](../internal/relay/admin.go#L212)）再关闭；清理仍走既有路径，符合 task.md §5 意图 | 语义仍成立（"只关句柄、不另写清理"），描述可更精确 |
| D5 | [README.md:551](../README.md#L551)（§10 #1） | 曾把"客户端自动重连缺失"与"协议热切换缺失"写在一起 | **阶段 6C/6E 已重写**：§10 #1 只讲自动重连（已实现），§10 #6 单列热切换（已实现） | 已关闭 |
| D6 | [README.md:742-748](../README.md#L742-L748)（§13.8） | 列 relay 页四块内容 | 实际 relay 页还有「概览」块（含 `UDP 承载模式`/`TCP 数据面`/`UDP 数据面`/`服务数`/`配对占位`/`UDP 会话数`，[admin.go:18-34](../internal/relay/admin.go#L18-L34)），task2 的验收正是靠这块 | 文档少列一块，非错误但漏了 task2 的关键证据 |

---

## 附录 A：生产代码日志 `event` 清单（共 68 个）

取证方式：对 `internal/**`（排除 `_test.go`）匹配 `"event", "<name>"` 后去重排序。

```
admin_action_duplicate  admin_action_invalid  admin_page_ignored  admin_render_error
admin_serve_error       admin_shutdown        admin_start         agent_gone
auth_failed             bad_request           bind                bind_fail
bind_ok                 config_warning        connect_request     control_close
control_connect         control_ignore        control_message     control_message_invalid
data_addr               data_close            data_open           error
exit                    heartbeat             heartbeat_timeout   hello_fail
hello_ok                idle_timeout          incoming            internal
kick                    kicked                listen              local_accept
message_ignored         pair_complete         pair_fail           pair_issue
pair_result             pair_timeout          peer_error          relay_error
server_shutdown         service_event         service_list        service_missing
service_proto_mismatch  service_register      service_registered  service_resync
service_unregister      shutdown              start               subscribe
tunnel_accept           udp_evict             udp_frame_drop      udp_session_close
udp_session_open        upstream_dial_failed  version_mismatch
```

§7.6 要求的关键事件全部在内：连接建立（`tunnel_accept`/`control_connect`/`local_accept`）、
注册（`service_register`/`service_registered`）、通知（`incoming`/`pair_issue`）、
配对（`bind_ok`/`pair_complete`）、断开（`data_close`/`control_close`）、
超时清理（`pair_timeout`/`idle_timeout`/`udp_evict`/`heartbeat_timeout`）、踢出（`kick`/`kicked`）；v0.2.0 新增健壮性事件 `reconnect_scheduled`/`reconnect_ok`/`reconnect_failed`、`listen_hot_switch`、`tunnel_drop`、`bind_dup_drop`。

## 附录 B：本轮取证命令与环境

> 本轮实际在 PowerShell 下用 `Select-String` 取证；下面给出等价的 `grep` 形式，便于在 Linux/macOS 复现。

```text
go version                                  → go1.24.4 windows/amd64
git log --oneline -1                        → v0.2.0 发布提交（feat: v0.2.0 运行期健壮性与部署运维件）
git tag -l                                  → v0.1.0、v0.2.0
git status --porcelain                      → （提交后为空）
grep -rn "^type (PairingKey|PairingTable|ServiceRegistry|PairingToken|TunnelListener|TCPDataListener|UDPDataListener)" internal/
                                            → 7/7 命中（见 §4.1）
grep -rn "install|uninstall|IsWindowsService" internal/service/ main.go
                                            → 4 个子命令 + run + IsWindowsService（见 §1.10 §10.6）
grep -rn "服务注册表|在线 agent|配对表|UDP 会话表|本地端口映射|当前活跃" internal/*/
                                            → 三模式页面内容齐全（见 §1.5）
grep -rn '"event", "[a-z_]*"' internal/     → 68 个事件（见附录 A）
grep -rn --include=*_test.go "^func Test" .     → 242 个 func Test*（含 TestMain 与辅助函数）；test 包 32 个顶层用例
```

> 说明：阶段 6B 取证时未重新执行 `go test ./...`（分析类任务）；阶段 6E 已在 Windows 与 WSL2
> 全量执行 `go test ./... -count=1`、`go test -race ./... -count=1` 与 soak 短版，结果见 README §13.12/§13.15/§13.17。
