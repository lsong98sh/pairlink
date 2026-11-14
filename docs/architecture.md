# PairLink 架构说明

> 权威版本。阶段 2 的三个子代理按本文与 `docs/contract.md` 分工实现，不得改动已冻结包。

## 1. 设计目标与边界

- **单二进制三模式**：`-relay` / `-agent` / `-client`，读同一份 ini 的不同段。
- **控制面与数据面严格分离**：控制面跑自定义 JSON 帧信令；数据面是哑管道。
- **鉴权不可绕过**：隧道 `auth_token` + 数据面一次性令牌双重校验；数据面无令牌不放行。
- **本地服务零改造**：agent 连本地目标、client 开本地监听，第三方只看到普通 TCP/UDP。
- **资源清理可靠**：所有清理走"关闭连接 → 转发退出 → 表项删除"既有路径；
  超时清理由单一 ticker 驱动，不产生 per-connection goroutine。
- **不是 P2P**：所有流量经过 relay，relay 掌握配对关系。

## 2. 目录与包职责

| 路径 | 归属 | 职责 |
| --- | --- | --- |
| `main.go` | 阶段1（冻结） | flag 解析、模式选择、config 加载校验、signal context、调用三个 `Run`、Windows 服务子命令 |
| `internal/config` | 阶段1（冻结） | 自写 ini 解析、默认值、按模式校验、`service.<ID>` 有序映射 |
| `internal/logx` | 阶段1（冻结） | `log/slog` 构造、级别/格式、`Component`、文件日志 |
| `internal/protocol` | 阶段1（冻结） | 帧编解码、消息结构体、错误码、版本协商 |
| `internal/token` | 阶段1（冻结） | 一次性配对令牌（SHA-256 存储、常量时间比较、TTL） |
| `internal/pairing` | 阶段1（冻结） | `PairingKey`、`PairingTable`（两侧顺序不确定） |
| `internal/registry` | 阶段1（冻结） | `ServiceRegistry`、`AgentRegistry`、`UDPTable` |
| `internal/forward` | 阶段1（冻结） | `Bidirectional` 半关闭转发、空闲超时包装、ctx 看护 |
| `internal/admin` | 阶段1（冻结） | 管理页框架、统一布局、CSRF、pprof 挂载 |
| `internal/service` | 阶段1（冻结） | Windows SCM 集成（跨平台可编译） |
| `internal/relay` | 阶段2（独占） | relay 全部实现 |
| `internal/agent` | 阶段2（独占） | agent 全部实现 |
| `internal/client` | 阶段2（独占） | client 全部实现 |
| `internal/version` | 阶段4（新增） | 唯一版本号常量（`Version = "v0.2.0"`），main 与三模式管理页共用 |
| `docs/`、`README.md` | 阶段1（冻结，阶段3 追加验证结果） | 规范与使用说明 |

依赖方向（箭头表示 import）：

```mermaid
graph LR
  main --> config
  main --> logx
  main --> service
  main --> relay
  main --> agent
  main --> client
  relay --> config & logx & protocol & token & pairing & registry & forward & admin
  agent --> config & logx & protocol & token & pairing & forward & admin
  client --> config & logx & protocol & token & pairing & forward & admin
  token --> pairing
  registry --> pairing
  registry --> protocol
```

**禁止反向依赖**：阶段1 包不得 import `internal/relay`、`internal/agent`、`internal/client`。
三个模式包之间也不得互相 import（避免把另一个模式的配置段/状态带进来）。

## 3. 三种模式的进程内对象图

### 3.1 relay

```mermaid
graph TD
  M[main] --> R[relay.Run]
  R --> TL[TunnelListener<br/>tunnel_listen]
  R --> TD[TCPDataListener<br/>tcp_listen]
  R --> UD[UDPDataListener<br/>udp_listen 可选]
  R --> UC[UDP-over-TCP Carrier<br/>复用 tcp_listen]
  R --> AD[admin.Server<br/>manage_listen 可选]
  TL --> SR[registry.ServiceRegistry]
  TL --> AR[registry.AgentRegistry]
  TL --> PT[pairing.PairingTable]
  TL --> TK[token.Store]
  TD --> PT
  TD --> TK
  TD --> FWD[forward.Bidirectional]
  TD --> UC
  UD --> UT[registry.UDPTable]
  UD --> TK
  UC --> TK
  UC -->|按 session 转发| UC
  AD --> SR
  AD --> AR
  AD --> PT
  AD --> UT
  AR -->|踢出| TL
  PT -->|OnExpire 关连接| TD
```

- 三个监听器类型名**固定**：`TunnelListener`、`TCPDataListener`、`UDPDataListener`（spec 第 8 节），
  由阶段 2 在 `internal/relay` 中实现，命名不得改动。
- `TunnelListener` 负责控制面：一连接一 goroutine（读循环）+ 心跳写 goroutine（可选合并到写锁）。
- `TCPDataListener` 每 accept 一个连接就起一个 goroutine：读一帧 Bind → 按 `carrier` 分流：
  - `carrier` 为空/`tcp`：`PairingTable.Claim` → 成功后 `forward.Bidirectional`；
  - `carrier=udp-over-tcp`：校验令牌后把连接转为 UDP 隧道帧解析循环（可复用多个 UDP 会话）。
- `UDPDataListener` 只在配置了 `udp_listen` 时存在，是单 goroutine 读循环 + `UDPTable`；
  不 per-packet 起 goroutine。
- UDP over TCP 模式（未配置 `udp_listen`）不需要 UDP socket：所有 UDP 会话都在
  `tcp_listen` 的连接上以隧道帧复用，relay 按会话标识转发到对端承载连接。
- 管理页的"踢出"只调用 `registry.Agent.Control.Close()` 或数据连接 `Close()`，
  清理走既有路径，不另写清理逻辑。

### 3.2 agent

```mermaid
graph TD
  M[main] --> AG[agent.Run]
  AG --> CC[控制连接<br/>Hello/Register/Incoming/Heartbeat]
  AG --> HC[心跳 ticker]
  AG --> AD[admin.Server 可选]
  AG --> DP[数据面回连<br/>native UDP socket 或 TCP carrier]
  DP --> FWD[forward.Bidirectional]
  FWD --> LOCAL[本地目标<br/>net.Dial / net.DialUDP]
  AG --> UE[本地 UDP 会话表<br/>map[PairingKey]session]
  AG --> CAR[UDP over TCP 承载连接<br/>udp_mode=tcp_tunnel]
  CAR --> FWD
  AD --> CC
  AD --> UE
```

- 每个 `MsgIncoming` 起一个配对 goroutine（有上限保护），成功后进入转发；结束即退出。
- agent 同时维护多个服务：`cfg.Server.All()` 里的每个服务都注册到控制连接。
- agent 的 UDP 侧需要一个本地会话 map（配对键 → 本地 UDP socket 或 carrier session），
  用 `sync.Mutex` 保护，空闲超时用 `[server] udp_session_timeout`。
- `udp_mode=native`：每个 UDP 配对 `net.DialUDP` 到 `udp_listen`，发 Bind 数据报；
  `udp_mode=tcp_tunnel`：复用一条到 `tcp_listen` 的承载连接，首帧
  `Bind{carrier:"udp-over-tcp"}`，后续会话用 `protocol.WriteUDPTunnelFrame` 复用。

### 3.3 client

```mermaid
graph TD
  M[main] --> CL[client.Run]
  CL --> CC[控制连接<br/>Hello/List/Subscribe/PairResult]
  CL --> LS[本地监听器<br/>每 service.<ID> 一个 TCP/UDP listener]
  CL --> PM[本地连接表<br/>clientConnID -> 本地连接]
  LS --> PM
  PM --> DP[数据面回连 + MsgBind]
  PM --> FWD[forward.Bidirectional]
  CL --> AD[admin.Server 可选]
```

- `client_conn_id` **由 client 生成**（决策见 §7.2）；client 维护
  `connID → 本地连接/状态` 表，`MsgPairResult` 到达后取出连接发起数据面绑定。
- 本地监听失败（端口占用）应返回错误而不是静默跳过（阶段 2 决定是否 fatal；建议 fatal）。

## 4. 连接注册表：数据结构与并发保护

### 4.1 服务注册表 `ServiceRegistry`

```go
type ServiceRegistry struct {
    mu       sync.RWMutex          // 读多写少
    services map[string]ServiceEntry // key = service_id
    revision uint64                // 每次变更 +1
    subs     map[int]func(Event)   // 订阅者
    nextSubID int
    now      func() time.Time
}
```

- 读操作（`Get`/`List`/`ListByAgent`/`Revision`/`Len`）持 `RLock`；
- 写操作（`Register`/`Unregister`/`UnregisterAgent`）持 `Lock`，**在锁内**完成 map 变更与
  `revision++`，**在锁外**依次调用订阅回调（避免回调重入死锁）；
- 对外只返回值拷贝（`ServiceEntry` 是值类型，无指针泄漏）；
- `List` 每次按 `ID` 排序后返回新切片，调用方可随意修改。

### 4.2 agent 注册表 `AgentRegistry`

```go
type AgentRegistry struct {
    mu     sync.RWMutex
    agents map[string]*Agent
}
```

- `Add` 存**副本**（含 `Services` 切片深拷贝），调用方后续修改入参不影响注册表；
- `Get`/`List` 返回值拷贝；`Touch`/`SetServices` 持写锁原地更新；
- `Agent.Control` 是 `registry.Conn` 接口（`Close() error` + `RemoteAddr() net.Addr`），
  与 `net.Conn` 解耦，便于测试伪造，也便于管理页踢出。

### 4.3 待配对表 `PairingTable`

```go
type PairingTable struct {
    mu      sync.Mutex
    timeout time.Duration
    now     func() time.Time
    entries map[PairingKey]*entry   // agent/client, createdAt, deadline, completed
    OnExpire func(key PairingKey, side Side, peer any)
}
```

- 单把 `sync.Mutex`：临界区只做 map 操作，不做 IO；
- 时间全部来自 `now()`，测试可注入假时钟；
- **没有 per-entry timer**：超时由调用方单一 ticker 调 `Sweep(now)`；
- `OnExpire` 在锁外调用，用于关闭超时占位的连接；
- 完成的条目保留一个 `timeout` 窗口再被清理，用于拒绝重放（`ErrAlreadyPaired`）。

### 4.4 UDP 会话表 `UDPTable`

```go
type UDPTable struct {
    mu         sync.Mutex
    sessions   map[PairingKey]*udpSession
    byEndpoint map[netip.AddrPort]PairingKey  // 反查索引，O(1)
    now        func() time.Time
}
```

- 双向索引保证"按源地址反查"是 O(1)；`Remove`/`Sweep` 必须同时清理两个索引；
- `netip.AddrPort` 是可比较值类型，天然适合做 map key；IPv4-mapped 地址统一 `Unmap()`；
- 端点是值类型，快照返回拷贝；
- `Sweep(now, timeout)` 返回被清理的配对键，供上层发 `MsgKicked`/写日志。

### 4.5 令牌表 `token.Store`

```go
type Store struct {
    mu      sync.Mutex
    ttl     time.Duration
    now     func() time.Time
    entries map[string]*entry  // key = pairing key string
}
type entry struct { hash [32]byte; issuedAt, expiresAt time.Time; consumed bool }
```

- 只存 SHA-256；`Validate` 用 `subtle.ConstantTimeCompare`；
- `Consume` 幂等；`Sweep` 由单一 ticker 驱动；
- 令牌与配对键强绑定（map 索引即键，摘要必须匹配）。

### 4.6 UDP over TCP 承载表（relay，阶段 2 自建）

未配置 `udp_listen` 时，UDP 数据面复用 `tcp_listen`。阶段 2 的 relay 需要自己维护
（冻结包不提供，因为它不是通用能力）：

```go
type carrierSession struct {
    key          pairing.PairingKey
    agentCarrier *carrier // 可为 nil（未报到）
    clientCarrier *carrier
}
type carrier struct {
    conn     net.Conn
    out      *tunnelqueue.Queue // 阶段 6C：单 writer goroutine + 有界发送队列
}
// session -> carrierSession、carrier -> 该连接上的 session 集合
```

- 每 accept 一条 `carrier=udp-over-tcp` 的 TCP 连接，起一个读循环 goroutine + 一个 writer goroutine；
- 每收到一帧：`session` 为空 → 控制帧（`MsgError`/`MsgBindOK`）；非空 → 查会话表转发；
- 写对端承载连接**只入队**（`internal/tunnelqueue`）：读循环绝不因慢对端阻塞；整帧单次
  `Write`；控制帧走独立优先队列；队列满时丢弃并计数（`event=tunnel_drop`，限速 WARN）；
- 承载连接断开时删除其上所有会话登记，对端数据帧因查不到会话被丢弃；
- 会话半开超时用 `pair_timeout`，完整会话空闲用 `udp_session_timeout`。

## 5. goroutine 生命周期与清理规则

| 位置 | goroutine | 退出条件 | 清理动作 |
| --- | --- | --- | --- |
| 控制面连接 | 读循环 + 可选写循环 | 对端关闭 / 读错误 / `heartbeat_timeout` / ctx 取消 / `MsgKicked` | 关闭控制连接、`AgentRegistry.Remove`、`ServiceRegistry.UnregisterAgent`、关闭该 agent 下未完成数据连接、`PairingTable.Remove` |
| TCP 数据面连接 | 读 Bind 帧 goroutine（同步） | 读到帧 / 读超时（`pair_timeout`） | 校验失败即关连接；成功转交 `forward.Bidirectional` |
| 已配对转发 | `forward.Bidirectional` 内 2 个 copy + 1 个 ctx 看护 | 任一侧 EOF（另一侧排空后）/ 异常错误 / ctx 取消 / idle 超时 | 返回前关闭两端；返回后 `PairingTable.Remove(key)`；写日志 |
| 待配对清理 | 1 个 ticker（进程级） | ctx 取消 | 停止 ticker，`Sweep` 收尾 |
| 令牌清理 | 1 个 ticker（进程级，建议 1s） | ctx 取消 | 停止 ticker |
| UDP 数据面（原生） | 1 个读循环 + 1 个清理 ticker | ctx 取消 / 监听关闭 | 停止循环，`UDPTable.Sweep` |
| UDP over TCP 承载 | 每连接 1 个读循环 + 1 个 writer（`tunnelqueue`） | 连接关闭 / 帧非法 / ctx 取消 / 队列关闭 | `Queue.Close()` 先停止入队、排空（上限 2s）后关连接；删除该连接全部会话登记 |
| UDP over TCP 会话 | 不单独起 goroutine（仅表项） | 半开 `pair_timeout` / 空闲 `udp_session_timeout` | 删除表项、通知对端（可选） |
| 管理页 | `net/http` 内部 goroutine | `admin.Server.Shutdown` | `ctx` 取消即 Shutdown（框架已实现） |
| agent/client 心跳 | 每条控制连接 1 个 ticker（重连即重建） | 该连接结束 / ctx 取消 | `serveControl` 等它退出后再换连接（不泄漏） |
| agent/client 重连监督 | 1 个（`Run` 所在 goroutine） | ctx 取消 / 踢出 / 永久错误 | 退避等待可被 ctx 取消；返回即 `Run` 返回 |
| agent 本地 UDP | 每会话 1 个读循环 | 本端/对端关闭 / 空闲超时 | 删除本地会话表项、关闭 socket |

硬性规则：

1. **每个 goroutine 必须有明确的退出信号**（ctx、连接关闭或 ticker stop）。
2. **不在锁内做 IO**，尤其不在持锁时 `Close()` 对端（可能阻塞）。
3. `forward.Bidirectional` 返回即代表两个 copy goroutine 与看护 goroutine 都已退出，
   阶段 2 不得再为同一对连接起后台 goroutine。
4. 清理顺序统一为：**关连接 → 等转发返回 → 删表项**。禁止先删表项再关连接
   （否则新一轮同键报到会与旧连接抢配对）。
5. 阶段 3 会用 goroutine 计数 / pprof 验证无泄漏；阶段 2 自测也应覆盖。

## 6. 日志字段约定

统一用 `log/slog`（`internal/logx`），**不要自造格式**。Key 固定为：

| key | 类型 | 说明 |
| --- | --- | --- |
| `mode` | string | `relay` / `agent` / `client`（`logx.Config.Mode` 自动附加） |
| `agent_id` | string | relay 分配或对端声明的 agent ID |
| `client_id` | string | relay 分配的 client ID |
| `service_id` | string | 服务ID |
| `pairing_key` | string | `service_id/client_conn_id` |
| `remote` | string | 对端地址 `ip:port` |
| `event` | string | 事件名（稳定，便于检索），见下表 |
| `err` | error | 错误对象（slog 会输出 error 字符串） |
| `component` | string | `logx.Component` 附加，如 `tunnel`/`tcp-data`/`udp-data`/`admin` |
| `detail` | string | 补充信息（配置警告等） |

建议事件名（阶段 2 沿用，保持可检索）：

`start`、`exit`、`config_warning`、`admin_start`、`tunnel_accept`、`hello_ok`、`auth_failed`、
`version_mismatch`、`service_register`、`service_registered`、`service_unregister`、`service_list`、
`subscribe`、`service_resync`、`connect_request`、`pair_issue`、`bind_ok`、`bind_fail`、
`pair_complete`、`pair_timeout`、`pair_replay`、`data_addr`、`data_open`、`data_close`、
`idle_timeout`、`udp_session_open`、`udp_session_close`、`udp_evict`、`heartbeat_timeout`、
`kick`、`server_shutdown`、`service_start`、`service_stop`。

日志级别约定：

- `Debug`：逐包/逐帧细节（默认关闭）；
- `Info`：连接建立/注册/通知/配对/断开/踢出等关键事件；
- `Warn`：可恢复异常（配置不完整、令牌校验失败、重放、超时清理）；
- `Error`：不可恢复错误（监听失败、控制连接异常、内部错误）。

## 7. 关键设计决策（spec 第 11 节自由度）

### 7.1 客户端连接 ID 的生成方

**由 client 生成**：`client_conn_id` 为 8 字节 `crypto/rand` 的 hex（16 字符），满足
`[a-z0-9-_]{1,64}`。理由：client 在发起 `ConnectRequest` 前就需要把本地连接与 ID 绑定；
若由 relay 生成，需要多一个信令往返且要处理 ID 分配失败。

### 7.2 relay 侧会话 ID

`agent_id = "a-" + 16 hex`，`client_id = "c-" + 16 hex`，由 `crypto/rand` 生成。
仅用于日志、注册表与管理页展示，不参与鉴权。

### 7.3 管理页 HTML 组织

`internal/admin` 提供统一布局（内联 CSS、utf-8、顶栏 mode/version/运行时长、1s 自动刷新
可关闭）与页面注册机制；每个模式只写主体 HTML（`<section>` 片段），不重复写 `<html>`。
写操作只有 `Action`（默认 POST + 一次性 CSRF 令牌），第一阶段唯一写操作是 relay 的踢出。

### 7.4 令牌与超时策略

默认 `token_ttl=15s`、`pair_timeout=10s`、`idle_timeout=300s`、`udp_session_timeout=60s`、
`heartbeat_timeout=90s`；均可通过 ini 覆盖。UDP 的令牌复用语义见 `docs/protocol.md` §5.2。

### 7.5 日志输出与 Windows 服务

- 前台模式：stdout/stderr（slog text/json）。
- Windows 服务模式：日志落到 **exe 同目录的 `pairlink-<mode>.log`**（`service.LogPath`），
  同时把 `os.Stderr`/`os.Stdout` 指向该文件，保证各包内 `logx.New` 的日志也落盘。

### 7.6 TLS 策略（阶段 6G：E7 显式开关）

TLS 是否启用由**三层**决定，优先级从高到低：

1. **显式 `tls` 开关**：`[relay]` / `[server]` / `[client]` 各一个，取值 `auto`（默认）|
   `true`（强制 TLS）| `false`（强制明文）；大小写不敏感，只接受这三个字面量
   （`on` / `yes` / `1` 等一律启动校验失败）；
2. **`auto` 时按材料推断**：完全复刻阶段 1~6 的旧行为；
3. **校验材料**（`tls_cert`/`tls_key`、`tls_ca`、`tls_insecure_skip_verify`）只在允许 TLS 时生效。

#### relay 监听侧

| `tls` | 证书材料 | 结果 |
| --- | --- | --- |
| `auto`（默认） | `tls_cert` + `tls_key` 均非空 | `tls.Listen`（TLS 1.2+） |
| `auto` | 都为空 | 明文 + WARN |
| `auto` | 只有一个 | 明文 + WARN（必须成对） |
| `true` | `tls_cert` + `tls_key` 均非空 | `tls.Listen`（TLS 1.2+） |
| `true` | 任一为空 | **启动校验失败**（配置错误，提示补 `tls_cert`/`tls_key`） |
| `false` | 任意（含已配证书） | 明文；配了证书则 WARN（"证书被忽略"） |

文件不存在仍只 WARN（允许先写配置后拷证书），但 `true` 时路径**必须非空**。

#### agent/client 拨号侧

| `tls` | `tls_ca` | `tls_insecure_skip_verify` | 结果 |
| --- | --- | --- | --- |
| `auto`（默认） | 空 | false | **明文** |
| `auto` | 非空 | false | TLS，`RootCAs` 只含该 CA |
| `auto` | 空 | true | TLS，跳过校验 + WARN |
| `auto` | 非空 | true | TLS，`RootCAs` 只含该 CA 且跳过校验 + WARN |
| `true` | 非空 | false | TLS，`RootCAs` 只含该 CA |
| `true` | 空 | false | TLS，**`RootCAs = nil`（系统根证书）**，标准校验 |
| `true` | 任意 | true | TLS，跳过校验 + WARN（`tls=true` 时开关多余，但仍生效） |
| `false` | 任意 | 任意 | **明文**；配了材料则 WARN（`tls_ca` / insecure 被忽略） |

- `ServerName` 始终取 `relay_addr` 的 host，证书必须包含该主机名或 IP SAN。
  **公网 CA 场景的安全写法**：relay 用公网证书，agent/client 只写 `tls = true`（不配 `tls_ca`），
  由系统根证书完成标准校验，不需要也不应该打开 `tls_insecure_skip_verify`。
- 两侧配置不匹配（relay 明文而 agent/client TLS，或反之）→ TLS 握手失败（relay 侧表现为
  读帧失败），agent/client 的 `Run` 返回可读错误并退出；TLS 握手失败按永久错误处理（不重连），
  不做协议探测/自动降级。
- 数据面永远是明文哑管道（端到端加密由第三方协议自己完成）。

> **兼容性**：默认 `auto` 与阶段 6 之前 100% 一致，旧 ini 不改一个字也能跑。
> 注意 `auto` 下"没配 `tls_ca` = 明文"仍是历史行为；新部署建议显式写 `tls = true`，
> 让"漏配 CA 静默降级为明文"不再可能发生（`tls = true` 时用系统根证书或直接失败）。


### 7.7 UDP 数据面承载模式

`[relay] udp_listen` 是**可选**的，由此产生两种模式（由 relay 判定并下发 `udp_mode`）：

| 模式 | 触发条件 | 数据面 | 优点 | 缺点 |
| --- | --- | --- | --- | --- |
| `native` | 配置了 `udp_listen` | 原生 UDP 数据报 | 无队头阻塞、延迟最低 | 公网多开一个 UDP 端口 |
| `tcp_tunnel` | 未配置 `udp_listen` | 复用 `tcp_listen` 的 TCP 承载连接，UDP 隧道帧复用多个会话 | 少一个公网端口，穿透只放行 TCP 的防火墙更容易 | **队头阻塞**：一条 TCP 上的丢包/阻塞会拖慢所有复用会话；实时语音/游戏等敏感场景应配置 `udp_listen` |

- 两种模式的令牌、配对键、超时语义一致；agent/client 根据 `HelloOK` / `ServiceRegistered` /
  `ServiceList` 的 `udp_mode` 选择接入方式。
- 阶段 2 的 relay 在启动时派生模式：
  `if cfg.Relay.UDPListen != "" → native else tcp_tunnel`。
- `UDPTable` 只用于 `native`；`tcp_tunnel` 的会话表由 relay 自己维护（见 §4.6）。

### 7.8 运行期健壮性（阶段 6C）

| 机制 | 位置 | 要点 |
| --- | --- | --- |
| 控制连接自动重连 | `internal/agent/reconnect.go`、`internal/client/reconnect.go`、`internal/reconnect` | `[server]/[client] reconnect`（默认 true）；base 1s ×2、cap `reconnect_max_backoff`（默认 60s）、±20% 抖动；等待可被 ctx 取消；重连后重新 Hello + 重注册/重拉列表 + 重建心跳；重连期间关闭在途会话（`reason=reconnect`），不做迁移；踢出/停机/鉴权失败/版本不匹配/TLS 握手失败不重连 |
| 订阅 proto 热切换 | `internal/client/listener.go` | 关旧监听 → 原地址按新协议重开；失败则回滚旧协议（本地服务不消失）；在途连接不受影响；`service_missing` 的 TCP 占位在服务出现后被热切换纠正 |
| 承载连接发送队列 | `internal/tunnelqueue` | 单 writer goroutine + 有界队列（数据 1024 帧 / 8 MiB，控制 256 帧且优先）；读循环只入队；整帧单次 Write；入队即复制 payload；满则丢弃并限速 WARN（`event=tunnel_drop`）；关闭时先停止入队再排空（上限 2s）后关连接 |
| 原生 UDP 报到重传 | `internal/agent/data.go`、`internal/client/session_udp.go`、`internal/relay/udp_data.go` | 每 ~300ms 重发 Bind（最多 3 次）；relay 半开幂等重复回 BindOK；Full 后丢弃同一配对键的重复 Bind 帧（帧头嗅探，其余数据报不解析） |

重连的退出语义：只有 ctx 取消（返回 nil）、`MsgServerShutdown`（nil）、`MsgKicked` /
鉴权失败 / 版本不匹配 / TLS 握手失败 / `reconnect=false` 下的断开（返回错误）会结束 `Run`；
其余断开都进入退避重连，进程保持存活。

## 8. 可观测性

- `[common] pprof = true` 时在 `manage_listen` 挂 `/debug/pprof/`（`internal/admin` 已实现，
  使用独立 mux，不污染 `DefaultServeMux`）。生产环境建议关闭或只监听 `127.0.0.1`。
- 管理页展示运行时长；阶段 2 页面应展示注册表/配对表/会话表快照（`Snapshot`/`List`）。
- goroutine 泄漏排查：`curl http://127.0.0.1:9000/debug/pprof/goroutine?debug=1`，
  或阶段 3 的 `runtime.NumGoroutine` 对比。
