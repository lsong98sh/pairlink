# 任务：用 Go 开发 PairLink —— 跨局域网配对式中继系统

## 1. 项目概述

**PairLink** 是一个基于“配对键”的跨局域网中继系统，让两个不同局域网、均无公网 IP 的端点通过一台公网服务器通信。中继是**协议无关的字节流中转**，不解析上层协议，同一套系统承载 HTTP、远程桌面（RDP/VNC）、SSH 等任意 TCP/UDP 流量。

**它不是 P2P NAT 穿透工具**：所有流量经过中继服务器，连接由信令阶段下发的配对键匹配。这一点必须在 README 第一句写清。

**交付物是单个可执行文件**，通过子命令切换三种模式：

```
pairlink -relay  -config relay.ini
pairlink -agent  -config server.ini
pairlink -client -config client.ini
```

同一二进制，按子命令加载 ini 中对应配置段。三种模式均支持 Linux 与 Windows。

## 2. 三种模式职责

**relay 模式（公网服务器）**
监听三个业务端口，职责严格分离：
- **隧道端口（控制面）**：agent/client 注册、服务列表查询/订阅、接入请求、回连通知、心跳、错误响应。跑自定义信令协议，建议 TLS。
- **TCP 通信端口（数据面）**：纯字节流双向转发，校验一次性配对令牌，不得引入协议解析。
- **UDP 通信端口（数据面）**：按配对键维护会话映射转发数据报，同样校验令牌。

维护连接注册表：`服务ID → server 控制连接`、`配对键 → {server 数据连接, client 数据连接}`、待配对表、UDP 会话表。状态变更需并发安全（加锁或 channel 串行化）。

**agent 模式（服务提供者侧）**
- 连接 relay 隧道端口，声明服务：`服务ID`、`协议类型（tcp/udp）`、`本地目标地址`。
- 维持控制长连接，等待接入通知。
- 配置维护多组 `服务ID → tcp/udp:本地端口` 映射，本地第三方服务零改造。
- 收到通知后携带 `配对键 + 令牌` **新建连接**到 relay 对应协议的通信端口完成绑定。
- 绑定后在**回连 relay 的数据连接**与**到本地目标的连接**之间双向转发。

**client 模式（访问侧）**
- 连接 relay 隧道端口，获取服务列表（启动拉取 + 可选订阅变更）。
- 为配置中要访问的服务，在本机开对应本地监听端口（TCP/UDP）。
- 第三方客户端连本地端口时，client 向 relay 发起接入请求，走完整配对流程，在**本地监听连接**与**配对后的中继数据连接**之间双向转发。
- 第三方客户端无需改造，只连 `127.0.0.1:本地端口`。

## 3. 核心配对流程（TCP）

1. agent 注册服务，relay 记录 `服务ID → 控制连接`。
2. client 请求访问某服务ID。relay 生成 `客户端连接ID` 和 `配对键 = (服务ID, 客户端连接ID)`，以及一次性令牌。
3. relay 向 agent 控制连接下发接入通知（配对键、令牌），同时向 client 回复配对信息。
4. agent 新建 TCP 连接到通信端口，首条消息带 `配对键 + 令牌`；client 同样新建连接报到。
5. relay 用**待配对表**处理两侧到达顺序不确定：先到者占位等待，同配对键另一侧到达且令牌有效即完成绑定；超时未凑齐则关闭已到连接并清理。
6. 绑定后双向 `io.Copy` 转发，任一端关闭或超时即清理该配对键下全部资源。
7. 报到消息只在连接建立瞬间出现一次，之后字节全部原样透传。

**UDP**：双方各向 UDP 通信端口发**报到数据报**（配对键+令牌），relay 记录两侧 `(IP,Port)` 建立会话映射，后续按源地址反查配对键转发。需独立会话超时清理；第一版不做源地址漂移处理，超时即清理并要求重连。TCP 与 UDP 必须共用同一套令牌校验机制。

## 4. ini 配置格式

统一 ini。三种模式都读 `[common]` 和各自配置段。服务映射用点号键扁平写法。

```ini
[common]
manage_listen = 127.0.0.1:9000

[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen    = 0.0.0.0:7001
udp_listen    = 0.0.0.0:7002
tls_cert      = /etc/pairlink/cert.pem
tls_key       = /etc/pairlink/key.pem
pair_timeout  = 10s
idle_timeout  = 300s

[server]
relay_addr = relay.example.com:7000
token      = xxx
protocol   = tcp
local      = 127.0.0.1:8080
service.dsh   = tcp:18080
service.xxxxx = tcp:18081

[client]
relay_addr = relay.example.com:7000
token      = xxx
service.http-svc = 127.0.0.1:8080
service.rdp-svc  = 127.0.0.1:13389
```

语义约定（README 必须写清）：
- `[server]` 下 `protocol` + `local` 是主服务，**默认服务ID 为 `default`**；`service.<服务ID> = <协议>:<本地端口>` 是附加服务映射，协议取 `tcp` 或 `udp`。
- `[client]` 下 `service.<服务ID> = <本地监听地址>`，client 为每个条目在本机开监听并映射到该远端服务。
- `manage_listen` 三种模式都读，第一版只提供只读管理页。

## 5. 管理页面

- 三种模式都在 `manage_listen` 上提供 HTTP 服务和简单 HTML 管理页。
- **第一版只做查看，不做配置写入**。配置修改一律改 ini + 重启。
- relay 页面：服务注册表（服务ID、协议、在线状态、所属 agent）、在线 agent 列表、当前配对表、UDP 会话表、踢出操作。
- server 页面：本机已注册服务列表、到 relay 的控制连接状态、当前活跃数据连接。
- client 页面：relay 返回的可用服务列表、本地端口映射表、当前活跃配对。
- **踢出局是唯一写操作**（relay 页面）：关闭对应控制连接或配对连接句柄，让“关闭连接 → 转发退出 → 表项清理”既有路径统一处理，不另写清理逻辑。

## 6. 安全与加密

- 隧道端口信令使用 TLS；agent 与 client 侧用预置 CA 或证书校验。
- 通信端口保持哑管道，不终止、不解析上层协议。HTTPS/SSH 的端到端加密由第三方客户端与真实服务自行完成，中继透明转发。
- 一次性令牌与配对键绑定，短有效期，用后即焚，TCP/UDP 通信端口都校验。

## 7. 技术约束

- 语言 **Go**，优先标准库；第三方库须说明理由。
- 单二进制，`-relay` / `-agent` / `-client` 子命令切换，配置统一 ini，各模式只读自己的段。
- 路径处理用 `filepath`，支持 Linux 与 Windows，README 说明两平台运行方式和防火墙注意事项。
- 双向转发用 `io.Copy` 配合 `context` 管理生命周期，任一端关闭时另一方向及时释放，无 goroutine 泄漏。
- 连接注册表需在 README 说明数据结构与并发保护方式。
- 日志覆盖连接建立、注册、通知、配对、断开、超时清理、踢出等关键事件。

## 8. 代码命名约定

- 配对键：`PairingKey`
- 待配对表：`PairingTable`
- 服务注册表：`ServiceRegistry`
- 一次性令牌：`PairingToken`
- 三个监听器：`TunnelListener`、`TCPDataListener`、`UDPDataListener`

## 9. 交付物

1. 完整可编译 Go 项目源码，module 路径 `github.com/lsong98sh/pairlink`，产出单一二进制，Linux/Windows 均可编译运行。
2. `README.md`：架构说明、协议消息定义与状态机图、ini 全部字段说明（含 `service.<ID>` 点号键语义与主服务默认ID `default`）、三模式部署运行方式、管理页说明。第一句写明 PairLink 是配对式中继，不是 P2P NAT 穿透工具。
3. 端到端验证说明，至少覆盖：
   - 本地 HTTP 服务零改造，经 agent 注册、client 本地端口访问成功。
   - TCP echo 或 RDP 服务走通注册→通知→回连→配对→转发→断开清理。
   - UDP 服务走通报到、会话映射、转发、超时清理。
   - 同一 agent 同时注册多个服务；并发多组配对互不干扰。
   - 令牌错误/复用/过期、控制连接断开、踢出操作、空闲超时清理的验证结果。

## 10. 验收标准

- 本地第三方服务零改造即可通过整套系统被远端访问。
- TCP 与 UDP 各一个通信端口上支持多连接、多服务、多协议并发复用，靠配对键区分。
- 令牌异常均拒绝绑定；agent 掉线、踢出、超时后无僵尸配对和 goroutine 泄漏（pprof 或 goroutine 计数佐证）。
- 三种模式的 `manage_listen` 页面均可访问并正确展示各自状态。
- 单二进制在 Linux 与 Windows 上均能按三种子命令正常启动和运行。
- windows 需要提供服务注册，取消，启动，停止 功能 用 -service install/start/stop/uninstall
  service 名为 pairlink-<mode>

## 11. 实现自由度

以下由你决定并在 README 说明：客户端连接ID 的生成方、管理页 HTML 组织方式、令牌有效期策略、UDP 会话超时阈值、日志格式。选择时优先保证：**控制面与数据面职责清晰**、**鉴权不可绕过**、**本地第三方服务零改造**、**资源清理可靠**、**单二进制三模式边界干净**。

## 12. 版本与许可证

第一版打 `v0.1.0` 标签，许可证选 Apache-2.0。