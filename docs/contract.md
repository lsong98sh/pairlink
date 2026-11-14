# 阶段 2 契约（唯一协同依据）

> 本文精确到函数签名与行为边界。阶段 2 的 relay / agent / client 三个子代理**只能**依据本文
> 与 `docs/architecture.md`、`docs/protocol.md` 实现；如有冲突，以本文 + `docs/protocol.md` 为准，
> 并立即在最终报告中提出**契约变更请求**。
>
> 阶段 1 已冻结的包（`internal/config`、`logx`、`protocol`、`token`、`pairing`、`registry`、
> `forward`、`admin`、`service`）、`main.go`、`go.mod`/`go.sum`、`docs/`、`README.md`
> **阶段 2 不得修改**。

## 1. 目录所有权

| 目录 / 文件 | 阶段 2 权限 |
| --- | --- |
| `internal/relay/**`（含 `_test.go`） | relay 子代理**独占**，可任意创建/修改/删除 |
| `internal/agent/**`（含 `_test.go`） | agent 子代理**独占** |
| `internal/client/**`（含 `_test.go`） | client 子代理**独占** |
| `internal/config/**` | **禁止修改** |
| `internal/logx/**` | **禁止修改** |
| `internal/protocol/**` | **禁止修改** |
| `internal/token/**` | **禁止修改** |
| `internal/pairing/**` | **禁止修改** |
| `internal/registry/**` | **禁止修改** |
| `internal/forward/**` | **禁止修改** |
| `internal/admin/**` | **禁止修改** |
| `internal/service/**` | **禁止修改** |
| `main.go` | **禁止修改** |
| `go.mod` / `go.sum` | **禁止修改**（不得新增依赖） |
| `docs/**`、`README.md` | **禁止修改**（阶段 3 追加端到端验证；阶段 4 收尾） |
| `test/**` | 阶段 2 **不得创建**（阶段 3 负责端到端测试） |

冲突处理：若阶段 2 确实需要新增/修改冻结包的导出 API，只能在最终报告中列出
**"契约变更请求"**（文件、符号、新增签名、原因、影响面），由阶段 3/4 统一评估并修改。
禁止自行改冻结包——三个子代理并行时会互相踩。

## 2. 三个 `Run` 的确切签名

```go
// internal/relay
func Run(ctx context.Context, cfg *config.Config) error

// internal/agent
func Run(ctx context.Context, cfg *config.Config) error

// internal/client
func Run(ctx context.Context, cfg *config.Config) error
```

阶段 2 必须**整文件替换**当前的桩实现（`internal/relay/relay.go` 等），
保留 package 名与 `Run` 签名不变。

### 2.1 `Run` 的通用行为契约

1. `cfg` 已经由 `main.go` 完成 `config.Load` + `cfg.Validate(mode)`，`Run` **不得重新
   读取 ini 文件**，也不得修改 `cfg` 的字段（只读；需要派生值请复制到本地变量）。
2. `Run` **不得调用 `os.Exit`、不得 `log.Fatal`、不得启动自己的 signal 处理**；
   进程信号由 `main.go` 的 `signal.NotifyContext` 负责，`ctx` 取消即要求全部退出。
3. `Run` 返回语义（阶段 6C 起 agent/client 增加自动重连，见 §2.4 第 7 条 / §2.5 第 9 条）：
   - `ctx` 取消且清理完成 → 返回 `nil`（返回 `context.Canceled` 也可，`main` 视为正常退出）；
   - 启动失败（端口占用、TLS 证书/CA 错误、配置非法、relay 明确拒绝等**不可重试**错误）
     → 返回非 nil 错误，`main` 记日志并退出码 1；
   - 运行中不可恢复错误（本地监听异常、被踢出）→ 返回非 nil 错误；
   - **例外（阶段 6C）**：启用 `[server]/[client] reconnect`（默认 `true`）时，
     agent/client 的**可重试**失败（拨号失败、握手读超时、连接被对端关闭/重置）与
     运行期控制连接意外断开**不结束进程**，而是按指数退避重连；重连期间 `Run` 不返回。
     `reconnect = false` 时保持阶段 6C 之前的语义（上述失败直接返回错误）。
4. `Run` 负责在 `cfg.Common.ManageListen != ""` 时启动管理页（`admin.New` + `admin.AddPage` +
   `admin.Start(ctx)`），并在退出时 `Shutdown`。`manage_listen` 为空表示不启动管理页。
5. `Run` 负责**自己构造 logger**：`logx.New(logx.Config{Level: cfg.Common.LogLevel,
   Format: cfg.Common.LogFormat, Mode: "relay"|"agent"|"client"})`，必要时用
   `logx.Component` 派生子 logger。不要重新解析日志级别/格式。
6. `Run` 返回前必须确保自己启动的所有 goroutine 已退出（`main` 之后直接 `os.Exit`，
   不等 GC）。清理顺序遵循 `docs/architecture.md` §5：关连接 → 等转发返回 → 删表项。
7. 三个模式包**不得互相 import**，也不得 import `internal/service`（服务集成只在 `main` 层）。
8. 每个模式包必须有**自己的 `_test.go`**，覆盖关键流程（可用假连接/构造小报文），
   且 `go test -race ./internal/relay/...` 等必须通过。

### 2.2 relay.Run 的启动顺序（强制）

1. 构造 logger；
2. 构造共享状态：`registry.NewServiceRegistry()`、`registry.NewAgentRegistry()`、
   `pairing.NewTable(cfg.Relay.PairTimeout)`、`token.NewStore(cfg.Relay.TokenTTL)`、
   `registry.NewUDPTable()`（仅在 `udp_mode=native` 时需要）；
3. 先监听数据面（`tcp_listen`；若 `cfg.Relay.UDPListen != ""` 再监听 `udp_listen`）
   再监听隧道（`tunnel_listen`），避免 client 已拿到配对信息但数据面尚未就绪；
   **`udp_mode` 在启动时确定**：`UDPListen != "" → native`，否则 `tcp_tunnel`；
4. 启动 TCP 数据面 accept 循环（同一端口同时服务纯透传与 `carrier=udp-over-tcp` 承载连接）；
   `native` 模式再启动 UDP 读循环；
5. 启动隧道监听 accept 循环；
6. 可选：`admin.Server` 启动（`manage_listen`）；
7. 启动 ticker：`PairingTable.Sweep`（周期 `PairTimeout/2`，最小 100ms）、
   `token.Store.Sweep`（1s）、UDP 清理（1s；`native` 用 `UDPTable`，
   `tcp_tunnel` 清理自己的承载会话表，半开用 `pair_timeout`、空闲用 `udp_session_timeout`）、
   控制面心跳判死检查（周期建议 5s，依据 `LastSeen` + `cfg.Relay.HeartbeatTimeout`）；
8. `<-ctx.Done()` → 关闭监听、发送 `MsgServerShutdown`、关闭全部控制/数据/承载连接、
   `admin.Shutdown`、停止 ticker，最后返回。

TLS：`cfg.Relay.TLSCert`/`TLSKey` 均非空时隧道 `tls.Listen`（`tls.Config{MinVersion: tls.VersionTLS12}`）；
否则明文 `net.Listen`（`config.Validate` 已经产生 WARN）。

### 2.3 relay 的职责（spec §2）

- **TunnelListener（控制面）**：处理 `MsgHello`（`CheckVersion` → 校验 `AuthToken` →
  回 `MsgHelloOK` 并登记 agent/client）、`MsgRegisterService`/`MsgServiceRegistered`、
  `MsgListServices`/`MsgServiceList`、`MsgSubscribe`、`MsgConnectRequest`/`MsgPairResult`+
  `MsgIncoming`、`MsgHeartbeat`/`MsgHeartbeatAck`、错误与踢出。
  - `MsgHelloOK` 必须填 `udp_mode`、`tcp_data_port`、`udp_data_port`（native 才有值）、
    `heartbeat_seconds`、`agent_id`/`client_id`；
  - `MsgServiceRegistered` 与 `MsgServiceList` 也必须带 `udp_mode`（task2 明确要求）。
- **TCPDataListener（数据面）**：accept 后设 `pair_timeout` 读 deadline，读**恰好一帧**
  `MsgBind`；按 `Carrier` 分流：
  - `carrier` 为空或 `"tcp"`：纯透传。校验 `role`、`pairing.ParsePairingKey`、
    `token.Store.Validate`；然后 `PairingTable.Claim(key, side, conn)`：
    - `ErrWaiting` → 保持连接（**不要写任何字节**），继续等对端；
    - 完整 `Pair` → `token.Store.Consume(key)`、清除读 deadline、`forward.WithIdleTimeout`
      包装两端、`forward.Bidirectional(ctx, agentConn, clientConn)`；返回后
      `PairingTable.Remove(key)` 并记录 `data_close`；
    - `ErrTimeout`/`ErrDuplicateSide`/`ErrAlreadyPaired`/`ErrInvalid`/`ErrExpired`/`ErrConsumed`
      → 关闭该连接（**不得写错误帧**）。
  - `carrier == "udp-over-tcp"`（`udp_mode=tcp_tunnel`）：校验该首帧的 role/令牌后，把连接
    转为 **UDP 隧道帧解析循环**（`protocol.ReadUDPTunnelFrame`），用 §4.6 的规则登记/转发；
    该连接不再进入纯透传。
- **UDPDataListener（数据面，仅 `udp_mode=native`）**：单 goroutine 读循环；对已登记端点直接
  `LookupByEndpoint` 转发并 `Touch`；对未知端点按帧解析 `MsgBind`，校验令牌 →
  `UDPTable.RegisterHalf` → 回 `MsgBindOK`；首次 `full=true` 时 `token.Store.Consume`。
  半会话过期用 `pair_timeout`，完整会话用 `udp_session_timeout`。UDP 读循环**不得
  per-packet 起 goroutine**。
- **UDP over TCP 承载（`udp_mode=tcp_tunnel`）**：见 §4.6；承载连接的读循环、写串行化、
  会话表、超时清理都由 relay 自己实现（冻结包不提供该表）。
- **注册表一致性**：agent 控制连接断开（含心跳判死）→ `ServiceRegistry.UnregisterAgent` +
  `AgentRegistry.Remove` + 关闭其名下未完成配对（`PairingTable` 项 + 数据连接）。
- **订阅推送的一致性**（阶段 4 明确）：订阅成功后 relay 立即下发一次**全量**
  `MsgServiceList` 作为基线；之后只在 `revision` **严格连续**时推单条 `MsgServiceEvent`。
  一旦发现 revision 断档或乱序（推送队列满丢弃、多个写入者并发 dispatch），relay 改发
  一次全量 `MsgServiceList`，client 必须能接受任意时刻主动下发的全量列表并整体替换视图。
  这样即使丢事件，client 也不会永久停留在旧视图。
- **踢出**：`Agent.Control.Close()` 或数据连接 `Close()`，让既有清理路径处理，不另写清理。
- **令牌校验与 `PairingTable` 的绑定规则**：`token.Store` 以 `pairing.PairingKey` 为索引；
  签发与校验都必须使用同一个 `pairing.ParsePairingKey` 解析出的键。

### 2.4 agent.Run 的职责

1. `tls`/明文连接 `cfg.Server.RelayAddr`（`cfg.Server.DialTimeout`）；TLS 配置规则见
   `docs/architecture.md` §7.6（阶段 6G 起为三态 `tls`：`true` 强制 TLS——`tls_ca` 非空时
   只信任该 CA，为空时用系统根证书；`false` 强制明文；`auto` 保持旧推断：`tls_ca` 非空或
   `tls_insecure_skip_verify=true` 才 TLS，否则**明文**）。`ServerName` 从 `relay_addr`
   的主机名取。
2. 发 `MsgHello{role:"agent", token: cfg.Server.Token, version: protocol.ProtocolVersion,
   client_name: 主机名}`；等 `MsgHelloOK`；`CheckVersion` 校验返回版本，并记录
   `HelloOK.UDPMode`（未识别值回退 `native` 并 WARN）。
3. 对 `cfg.Server.All()` 的每个服务发 `MsgRegisterService{service_id, proto, local_hint: svc.Addr}`
   并等待 `MsgServiceRegistered`（或 `MsgError` → 返回错误）；同时记录响应里的 `udp_mode`
   （以最后一次响应为准，缺失时沿用 HelloOK 的值）。
4. 启动心跳 ticker（间隔 `min(cfg.Server.Heartbeat, HelloOK.HeartbeatSeconds)`），
   并发读控制连接。
5. 收到 `MsgIncoming`：解析配对键（`pairing.ParsePairingKey`），按 `proto`：
   - `tcp`：按 §4.5 从 `cfg.Server.RelayAddr` 的 host + `cfg.Relay.TCPListen` 的 port 推导
     relay 数据面地址，`net.DialTimeout` 连接后写 `MsgBind{key, token, role:"agent"}`，
     然后 `forward.Bidirectional(ctx, dataConn, upstreamConn)`；
   - `udp`：按 §4.6 选择承载方式（`native` / `tcp_tunnel`），完成报到后与本地目标做 UDP
     转发（维护本地 UDP 会话表，空闲超时 `cfg.Server.UDPSessionTimeout`）。
6. 收到 `MsgKicked`/`MsgServerShutdown`：关闭对应数据连接；`MsgKicked` 时 `Run` 返回错误
   （**不重连**），`MsgServerShutdown` 时返回 `nil`（由外部守护重启）。
7. 控制连接意外断开（阶段 6C 起）：`[server] reconnect`（默认 `true`）时**自动重连**，
   `reconnect = false` 时保持旧行为（`Run` 返回错误，进程退出，由 systemd/Windows
   服务恢复策略重启）。
   - 退避：base **1s**、每次 ×2、上限 `[server] reconnect_max_backoff`（默认 **60s**，
     必须 > 0）、±20% 抖动；等待必须可被 `ctx` 取消（取消即 `Run` 返回 `nil`）；
   - 每次重连成功后**重新 `MsgHello`、重新注册 `cfg.Server.All()` 的全部服务、重建心跳**；
     relay 在旧控制连接断开时已注销本 agent 的服务，因此重注册是必需的；
   - 重连期间关闭全部在途数据连接与本地 UDP 会话（日志 `reason=reconnect`），
     **不做 UDP/TCP 会话迁移**：重连后的新接入照常走完整配对；
   - 可观测：`event=reconnect_scheduled`（含 attempt/delay）、`event=reconnect_ok`
     （含 attempt/elapsed）、`event=reconnect_failed`（含 err）；日志不得含令牌；
   - **不重连的情况**：`MsgKicked`（清理后返回错误）、`MsgServerShutdown`（返回 `nil`）、
     鉴权失败 `auth_failed` / 版本不兼容 `unsupported_version` / 注册被 relay 明确拒绝
     （均返回错误，重试无意义）、TLS 握手失败（配置不匹配）、`ctx` 取消（返回 `nil`）。
8. 本地目标连接失败只影响本次配对（关数据连接 + 日志），不得让整个 `Run` 退出。

### 2.5 client.Run 的职责

1. 连接 relay 隧道（TLS 规则同 agent），`MsgHello{role:"client", token: cfg.Client.Token, ...}`，
   校验 `MsgHelloOK` 的版本，并记录 `UDPMode`（未识别值回退 `native` 并 WARN）。
2. `MsgListServices` 拉取列表（用于管理页展示与订阅基线），记录 `ServiceList.UDPMode`。
3. `cfg.Client.Subscribe == true` 时发 `MsgSubscribe{Enable:true}` 并处理 `MsgServiceEvent`
   刷新本地视图（`msg.Revision` 小于已见版本时丢弃）。
4. 对 `cfg.Client.Services` 每个条目开本地监听。协议**以 relay 返回的服务列表为准**：
   - `Run` 先完成 `MsgHelloOK` 与 `MsgListServices`，再开监听；
   - 列表中存在该 `service_id` → 按 `proto` 开 TCP/UDP 监听；不存在 → 按 **TCP** 开监听并
     记 WARN（`event=service_missing`），`ConnectRequest` 失败时关闭本地连接；
   - 订阅开启时，收到 `added`/`updated` 事件且 proto 与当前监听不一致：**热切换**
     （见第 10 条），不再只是 WARN。
5. 本地连接建立后生成 `client_conn_id`（8 字节 `crypto/rand` hex），登记到本地表，
   发 `MsgConnectRequest`；启动 `cfg.Client.PairTimeout` 定时器，超时则关闭本地连接。
6. 收到 `MsgPairResult`：按 `proto` 选择数据面（§4.5/§4.6）：
   - `tcp`：连 `tcp_listen`，首帧 `MsgBind{key, token, role:"client"}`，然后
     `forward.Bidirectional(ctx, localConn, dataConn)`；
   - `udp`：按 `udp_mode` 走原生 UDP 或 TCP 承载（§4.6），与本地 UDP 会话双向转发。
   转发结束删除本地表项。
7. 收到 `MsgError`（`service_not_found`/`no_agent_online`/`pair_timeout`/`too_many_pending`）：
   关闭对应本地连接、记 WARN，不影响其它连接。
8. 本地监听失败 → `Run` 返回错误（端口冲突必须暴露，不能静默）；热切换时新监听打开失败
   则**回滚**旧监听（见第 9 条第 3 点），不结束 `Run`。
9. 控制连接（阶段 6C 起，与 agent 对称）：
   - `[client] reconnect`（默认 `true`）时自动重连：base 1s、×2、
     上限 `[client] reconnect_max_backoff`（默认 60s，必须 > 0）、±20% 抖动，
     等待可被 `ctx` 取消；
   - 重连成功后重新 `MsgHello`、重新 `MsgListServices`、重新 `MsgSubscribe`（若启用），
     刷新本地服务视图与 `udp_mode`；**本地监听不重建**（端口不变、第三方无感），
     只按新列表校对协议（必要时热切换）；
   - 重连后必须**强制接受** relay 下发的全量服务列表作为新基线（relay 重启会导致
     `revision` 归零；沿用"忽略更旧 revision"会让视图永久卡死）；
   - 重连期间关闭全部在途本地 TCP 连接/UDP 会话（`reason=reconnect`），不做迁移；
   - 不重连：`MsgKicked`（返回错误）、`MsgServerShutdown`（返回 `nil`）、
     鉴权失败/版本不兼容/TLS 握手失败/`tls_ca` 读取失败（永久错误）、`ctx` 取消（返回 `nil`）；
   - `reconnect = false` 时控制连接断开 → `Run` 返回错误（旧行为）。
10. 订阅到服务 `proto` 变化时**热切换本地监听**（阶段 6C）：
    - 关旧监听 → 在**原地址**按新协议重开（配置为 `:0` 时用旧监听的实际端口）；
      成功则替换映射并记 `event=listen_hot_switch`；
    - 新监听打开失败 → **回滚**：按旧协议在原地址重开，保留旧映射并 WARN，
      继续用旧协议（绝不让本地服务消失）；只有回滚也失败才移除映射并 WARN；
    - 切换只关监听 socket，已建立的会话各自持有连接，允许自然结束；新连接走新协议；
    - 服务不在列表里时保留当前监听协议（`service_missing` 的 TCP 占位只用于首次启动），
      服务重新出现且 proto 不同则再次热切换。

## 3. 冻结包的可用 API（阶段 2 只允许用这些）

### 3.1 `internal/config`

```go
type Mode string
const (
    ModeRelay  Mode = "relay"
    ModeAgent  Mode = "agent"
    ModeClient Mode = "client"
)
const DefaultServiceID = "default"
func ParseMode(s string) (Mode, error)
func (m Mode) Valid() bool
func (m Mode) String() string

type Config struct {
    Path     string
    Common   CommonConfig
    Relay    RelayConfig
    Server   ServerConfig
    Client   ClientConfig
    Warnings []string
}
func Load(path string) (*Config, error)
func (c *Config) Validate(mode Mode) error

type CommonConfig struct {
    ManageListen, LogLevel, LogFormat string
    Pprof bool
}
type RelayConfig struct {
    TunnelListen, TCPListen, UDPListen, TLSCert, TLSKey, AuthToken string
    PairTimeout, IdleTimeout, HeartbeatTimeout, UDPSessionTimeout, TokenTTL time.Duration
}
type ServerService struct{ ID, Proto, Addr string }
type ServerConfig struct {
    RelayAddr, Token, Protocol, Local string
    Services []ServerService
    TLSCA string
    TLSInsecureSkipVerify bool
    Heartbeat, DialTimeout, UDPSessionTimeout time.Duration
}
func (c ServerConfig) All() []ServerService   // 主服务 default 在前
type ClientService struct{ ID, Listen string }
type ClientConfig struct {
    RelayAddr, Token string
    Services []ClientService
    TLSCA string
    TLSInsecureSkipVerify bool
    Heartbeat time.Duration
    Subscribe bool
    DialTimeout, PairTimeout time.Duration
    UDPSessionTimeout time.Duration // 阶段 4 新增：[client] udp_session_timeout，默认 60s
}
```

### 3.2 `internal/logx`

```go
type Config struct {
    Level, Format string
    Output io.Writer
    Mode, Component string
}
func ParseLevel(s string) (slog.Level, error)
func New(cfg Config) *slog.Logger
func Component(log *slog.Logger, name string) *slog.Logger
func NewFileLogger(path string, cfg Config) (*slog.Logger, io.Closer, error)
```

### 3.3 `internal/protocol`

```go
const ProtocolVersion = 1
const MaxPayload = 1 << 20
const HeaderSize = 5
const (
    MaxUDPDatagram       = 65535
    MaxSessionIDLen      = 129
    MaxUDPTunnelFrameLen = 1 + MaxSessionIDLen + MaxUDPDatagram
)

type MessageType uint8
const (
    MsgHello, MsgHelloOK, MsgRegisterService, MsgServiceRegistered,
    MsgListServices, MsgServiceList, MsgSubscribe, MsgServiceEvent,
    MsgConnectRequest, MsgPairResult, MsgIncoming, MsgBind, MsgBindOK,
    MsgHeartbeat, MsgHeartbeatAck, MsgError, MsgKicked, MsgServerShutdown MessageType = ...
)
func (t MessageType) String() string
func (t MessageType) Valid() bool

func WriteFrame(w io.Writer, typ MessageType, payload any) error
func ReadFrame(r io.Reader) (MessageType, []byte, error)
func ReadFrameWithLimit(r io.Reader, limit int) (MessageType, []byte, error)
func EncodeFrame(typ MessageType, payload any) ([]byte, error)       // 帧 → 字节（可塞进数据报/隧道帧）
func ParseFrame(data []byte) (MessageType, []byte, error)           // 字节 → 帧（要求无尾随字节）
func Decode(typ MessageType, payload []byte) (any, error)           // 返回 *Hello 等指针
func CheckVersion(v int) error

// UDP over TCP 隧道帧（§4.6 / docs/protocol.md §2.1）
func WriteUDPTunnelFrame(w io.Writer, session string, payload []byte) error
func ReadUDPTunnelFrame(r io.Reader) (session string, payload []byte, err error)

// 消息结构体（全部字段见 docs/protocol.md §4）
Hello, HelloOK, RegisterService, ServiceRegistered, ListServices, ServiceList,
Subscribe, ServiceEvent, ConnectRequest, PairResult, Incoming, Bind, BindOK,
Heartbeat, HeartbeatAck, Error, Kicked, ServerShutdown

// Error 新增可选关联字段（阶段 4，向后兼容、不升版本）：
//   ClientConnID string  // relay 应答某个 ConnectRequest 的错误时填入其 client_conn_id；

// HelloOK 额外的数据面协商字段：
//   UDPMode string, TCPDataPort int, UDPDataPort int
// Bind 额外的承载字段：
//   Carrier string  // ""/"tcp" 纯透传；"udp-over-tcp" UDP over TCP

const RoleAgent = "agent"; RoleClient = "client"
const UDPModeNative = "native"; UDPModeTCPTunnel = "tcp_tunnel"
const CarrierTCP = "tcp"; CarrierUDPOverTCP = "udp-over-tcp"
const EventAdded = "added"; EventRemoved = "removed"; EventUpdated = "updated"
const (
    ErrCodeAuthFailed, ErrCodeServiceNotFound, ErrCodeNoAgentOnline,
    ErrCodePairTimeout, ErrCodeInvalidToken, ErrCodeAlreadyPaired,
    ErrCodeTooManyPending, ErrCodeInternal, ErrCodeUnsupportedVersion,
    ErrCodeBadRequest string = ...
)
var ErrPayloadTooLarge, ErrUnknownType, ErrUnsupportedVersion, ErrBadUDPTunnelFrame error
```

`Decode` 返回类型断言示例：`msg, _ := protocol.Decode(typ, payload); hello := msg.(*protocol.Hello)`。

### 3.4 `internal/token`

```go
var ErrInvalid, ErrExpired, ErrConsumed error
type Store struct{ /* ... */ }
type PairingToken = Store
func NewStore(ttl time.Duration) *Store
func (s *Store) TTL() time.Duration
func (s *Store) Issue(key pairing.PairingKey) (string, error)
func (s *Store) Validate(key pairing.PairingKey, tok string) error
func (s *Store) Consume(key pairing.PairingKey)
func (s *Store) Sweep() int
func (s *Store) Len() int
func (s *Store) SetClock(now func() time.Time) // 仅测试
```

### 3.5 `internal/pairing`

```go
type PairingKey struct{ ServiceID, ClientConnID string }
func (k PairingKey) String() string          // "serviceID/clientConnID"
func (k PairingKey) IsZero() bool
func (k PairingKey) Validate() error
func ParsePairingKey(s string) (PairingKey, error)

type Side int
const (SideUnknown Side = iota; SideAgent; SideClient)
func (s Side) Valid() bool
func (s Side) String() string

type Pair struct{ Key PairingKey; Agent, Client any }
type PairingSnapshot struct {
    Key PairingKey
    AgentArrived, ClientArrived, Completed bool
    CreatedAt, Deadline time.Time
    Remaining time.Duration
}

var ErrWaiting, ErrTimeout, ErrDuplicateSide, ErrAlreadyPaired, ErrInvalidSide error

func NewTable(pairTimeout time.Duration) *PairingTable
func (t *PairingTable) Timeout() time.Duration
func (t *PairingTable) Claim(key PairingKey, side Side, peer any) (Pair, error)
func (t *PairingTable) Remove(key PairingKey)
func (t *PairingTable) Sweep(now time.Time) int
func (t *PairingTable) Snapshot() []PairingSnapshot
func (t *PairingTable) Len() int
func (t *PairingTable) SetClock(now func() time.Time)
func (t *PairingTable) SetOnExpire(fn func(key PairingKey, side Side, peer any))
// 字段（建议用 SetOnExpire 设置）：OnExpire func(key PairingKey, side Side, peer any)
```

`Claim` 第一次调用返回 `(Pair{}, ErrWaiting)`；完成时返回 `(Pair{Key, Agent, Client}, nil)`。

### 3.6 `internal/registry`

```go
type Conn interface {
    Close() error
    RemoteAddr() net.Addr
}
type Agent struct {
    ID, RemoteAddr string
    ConnectedAt, LastSeen time.Time
    Services []string
    Control Conn
}

var (
    ErrServiceNotFound, ErrServiceOwnerMismatch, ErrAgentExists,
    ErrAgentNotFound, ErrUDPEndpointInvalid, ErrUDPEndpointConflict error
)

type ServiceEntry struct {
    ID, Proto, AgentID, AgentAddr string
    Online bool
    RegisteredAt time.Time
}
type Event struct {
    Type string
    Service ServiceEntry
    Revision uint64
    At time.Time
}
const (
    EventAdded   = protocol.EventAdded   // "added"
    EventRemoved = protocol.EventRemoved // "removed"
    EventUpdated = protocol.EventUpdated // "updated"
)
func NewServiceRegistry() *ServiceRegistry
func (r *ServiceRegistry) Register(e ServiceEntry) error
func (r *ServiceRegistry) Unregister(serviceID, agentID string) error
func (r *ServiceRegistry) UnregisterAgent(agentID string) []string
func (r *ServiceRegistry) Get(serviceID string) (ServiceEntry, bool)
func (r *ServiceRegistry) List() []ServiceEntry
func (r *ServiceRegistry) ListByAgent(agentID string) []ServiceEntry
func (r *ServiceRegistry) Revision() uint64
func (r *ServiceRegistry) Len() int
func (r *ServiceRegistry) Subscribe(fn func(Event)) (cancel func())
func (r *ServiceRegistry) SetClock(now func() time.Time)

func NewAgentRegistry() *AgentRegistry
func (r *AgentRegistry) Add(a *Agent) error
func (r *AgentRegistry) Remove(agentID string) error
func (r *AgentRegistry) Get(agentID string) (Agent, bool)
func (r *AgentRegistry) List() []Agent
func (r *AgentRegistry) Count() int
func (r *AgentRegistry) Touch(agentID string, now time.Time) error
func (r *AgentRegistry) SetServices(agentID string, services []string) error

type UDPEndpoint struct {
    IP   netip.Addr
    Port uint16
}
func (e UDPEndpoint) IsValid() bool
func (e UDPEndpoint) AddrPort() netip.AddrPort
func (e UDPEndpoint) String() string
func UDPEndpointFromAddrPort(ap netip.AddrPort) UDPEndpoint
func UDPEndpointFromString(s string) (UDPEndpoint, error)
func UDPEndpointFromNetAddr(a net.Addr) (UDPEndpoint, error)

type UDPEntry struct {
    Key PairingKey
    Agent, Client UDPEndpoint
    AgentReady, ClientReady, Complete bool
    CreatedAt, LastSeen time.Time
}
func NewUDPTable() *UDPTable
func (t *UDPTable) RegisterHalf(key pairing.PairingKey, side pairing.Side, ep UDPEndpoint) (full bool, peer UDPEndpoint, err error)
func (t *UDPTable) LookupByEndpoint(ep UDPEndpoint) (key pairing.PairingKey, peer UDPEndpoint, ok bool)
func (t *UDPTable) LookupByKey(key pairing.PairingKey) (UDPEntry, bool)
func (t *UDPTable) Touch(key pairing.PairingKey, at time.Time)
func (t *UDPTable) Remove(key pairing.PairingKey)
func (t *UDPTable) Sweep(now time.Time, timeout time.Duration) []pairing.PairingKey
func (t *UDPTable) Snapshot() []UDPEntry
func (t *UDPTable) Len() int
func (t *UDPTable) SetClock(now func() time.Time)
```

注意：
- `AgentRegistry.Add` 存**副本**；`Get`/`List` 返回副本，修改返回值不影响内部状态。
- `UDPTable.LookupByEndpoint` 对半注册也返回 `ok=true`，但 `peer` 为零值 → **不可转发**。
- `UDPTable.RegisterHalf` 传入裸 `net.UDPAddr` 时用 `UDPEndpointFromNetAddr` 转换
  （内部会 `Unmap()` IPv4-mapped 地址）。

### 3.7 `internal/forward`

```go
func Bidirectional(ctx context.Context, a, b io.ReadWriteCloser) (aToB, bToA int64)
func WithIdleTimeout(conn net.Conn, idle time.Duration) net.Conn
func JoinContext(ctx context.Context, conn io.Closer) (stop func())

// 阶段 4 新增（向后兼容，Bidirectional 行为不变）：
type Outcome struct {
    AToB, BToA int64
    AToBErr, BToAErr error
}
func (o Outcome) Err() error      // 第一个异常错误（正常 EOF/已关闭/ctx 取消 → nil）
func (o Outcome) TimedOut() bool  // 是否有方向因超时结束（WithIdleTimeout 空闲超时）
func BidirectionalWithOutcome(ctx context.Context, a, b io.ReadWriteCloser) Outcome
```

- `Bidirectional` 返回即关闭两端、且所有内部 goroutine 已退出；不要重复使用连接。
- `WithIdleTimeout` 包装后仍支持 `CloseWrite`（半关闭）。
- 传入 `nil` 连接返回 `(0, 0)`。
- relay 用 `BidirectionalWithOutcome` 区分"正常关闭"与"空闲超时"，后者记
  `event=idle_timeout` 日志。

### 3.8 `internal/admin`

```go
type Action struct {
    Name, Title, Method string
    Handler func(http.ResponseWriter, *http.Request)
}
type Page struct {
    Path, Title string
    Render func(w io.Writer, r *http.Request)
}
type Options struct {
    Title, Mode, Version string
    Logger *slog.Logger
    Pprof bool
    Actions []Action
}
func New(addr string, opts Options) *Server
func (s *Server) Start(ctx context.Context) error   // 同步监听、后台 Serve；ctx 取消自动 Shutdown
func (s *Server) Addr() string
func (s *Server) Shutdown(ctx context.Context) error
func (s *Server) AddPage(p Page)                    // 必须在 Start 之前调用
func (s *Server) Pages() []Page
func (s *Server) Handler() http.Handler
func (s *Server) CSRFToken() string
func (s *Server) CSRFInput() template.HTML
func (s *Server) WriteHTML(w http.ResponseWriter, r *http.Request, pageBody template.HTML)
var ErrAlreadyStarted error
```

- 页面只写主体 HTML（`<section>…`），`Render` 内**不要**写 `<html>`/`<head>`/`<body>`。
- Action 的 CSRF 校验由框架完成；Handler 内直接用 `r.PostFormValue(...)` 即可。
- 管理页路径 `/` 为内置首页（自动列出页面与操作），`/action/<Name>` 为写操作，
  `/debug/pprof/` 仅在 `Pprof=true` 时存在。
- 三个模式注册的页面必须覆盖 spec §5 的内容：
  - relay：服务注册表、在线 agent、当前配对表、UDP 会话表、踢出操作；
  - agent：本机已注册服务、到 relay 的控制连接状态、当前活跃数据连接；
  - client：relay 返回的可用服务列表、本地端口映射表、当前活跃配对。

### 3.9 `internal/service`

```go
var ErrUnsupported error
type State int
const (StateUnknown State = iota; StateStopped; StateStartPending; StateStopPending; StateRunning; StatePaused)
func (s State) String() string
type Status struct{ State State; PID uint32 }
func Name(mode string) string      // "pairlink-<mode>"
func LogPath(name string) (string, error)
func Install(name, displayName string, args []string) error
func Uninstall(name string) error
func Start(name string) error
func Stop(name string) error
func Query(name string) (Status, error)
func Run(name string, run func(ctx context.Context) error) error
func IsWindowsService() bool
```

阶段 2 **不直接使用**该包（`main.go` 已接线）。

## 4. 三个模式共同遵守的语义规则

### 4.1 ID 与配对键格式

- `service_id`：`[a-z0-9-_]{1,64}`，`default` 保留给 `[server]` 主服务。
- `client_conn_id`：client 用 `crypto/rand` 生成 8 字节 hex（16 字符）。
- 配对键字符串：`"<service_id>/<client_conn_id>"`，解析必须用 `pairing.ParsePairingKey`。
- relay 生成 `agent_id = "a-"+16hex`、`client_id = "c-"+16hex`。

### 4.2 令牌

- 只有 relay 的 `token.Store` 签发/校验；agent/client 只透传 `pairing_token` 字符串。
- relay 在**配对完成**时 `Consume(key)`；UDP 在会话 `Full` 时 `Consume(key)`。
- 校验失败一律不透露具体原因（日志可记录 `errors.Is` 结果）。

### 4.3 TLS

- **三态开关**（阶段 6G / E7）：`[relay]`、`[server]`、`[client]` 各有一个 `tls` 键，
  取值 `auto`（默认）| `true` | `false`，大小写不敏感；非法值在配置校验阶段直接报错。
  `auto` 完全保持阶段 1~6 的推断行为，旧 ini 无需改动。
- relay：
  - `auto`：`tls_cert`+`tls_key` 均非空 → `tls.Listen`（TLS 1.2+）；否则明文 + WARN；
  - `true`：必须 TLS，`tls_cert`/`tls_key` 任一为空 → `Validate` 报错（文件不存在仍只 WARN）；
  - `false`：一律明文；若配了证书则 WARN（材料被忽略）。
- agent/client：
  - `auto`：`tls_ca` 非空 → TLS 且 `RootCAs` 只含该 CA；仅 `tls_insecure_skip_verify=true`
    → TLS 但跳过校验（WARN）；两者皆空 → **明文**（与旧版本一致）；
  - `true`：必须 TLS。`tls_ca` 非空 → `RootCAs` 只含该 CA；`tls_ca` 为空 →
    `RootCAs = nil`（**系统根证书**标准校验，不需要 `tls_insecure_skip_verify`）。
    `tls_insecure_skip_verify=true` 优先级最高，仍跳过校验并 WARN（`tls=true` 时该开关多余）。
  - `false`：一律明文；若配了 `tls_ca` / `tls_insecure_skip_verify` 则 WARN（被忽略）。
  - `ServerName` 始终取 `relay_addr` 的 host。公网 CA 场景写 `tls = true`、不配 `tls_ca`。
- 两侧配置不匹配（relay 有证书而 agent/client 明文，或反之）时 TLS 握手失败，属**预期**行为：
  不做协议探测、不自动降级，错误信息需可读（提示检查 relay 的 tls 与本端 tls/tls_ca）。
  阶段 6C 起：本端 TLS **握手**失败（`tls_ca` 与 relay 证书不匹配、relay 没开 TLS、
  `tls=true` 但系统根证书拒绝自签证书）按**永久错误**处理，立即返回 `Run` 错误（不重试）；
  而"relay 要求 TLS、本端明文"只能表现为读 HelloOK 时 EOF，与瞬时断链不可区分，因此按可
  重试断开处理（`reconnect=false` 时仍立即失败退出，见 §2.5 第 9 条）。
- 数据面**永远明文**（哑管道），不得包 TLS。

### 4.4 超时使用

- relay TCP 数据面读 Bind 帧：`SetReadDeadline(now+pair_timeout)`，绑定成功后
  `SetDeadline(time.Time{})` 清除，再交给 `forward.WithIdleTimeout(conn, cfg.Relay.IdleTimeout)`。
- TCP 转发两端都用 `WithIdleTimeout`（`idle_timeout`）。
- UDP：半会话 `pair_timeout`，完整会话 `udp_session_timeout`。
- 控制面心跳：间隔 `heartbeat`，relay 用 `heartbeat_timeout` 判死。
- client 等待配对结果：`cfg.Client.PairTimeout`。
- 所有 `time.After`/`time.Tick` 都必须受 ctx 或 ticker `Stop()` 约束，不能泄漏。

### 4.5 数据面地址推导（重要，避免三个模式各写一套）

`[relay] tcp_listen` / `udp_listen` 是 **relay 的监听地址**；agent/client 的 ini 里通常没有
`[relay]` 段，因此**数据面端口由 relay 在 `MsgHelloOK` 中下发**：

```go
// host 来自 cfg.Server.RelayAddr / cfg.Client.RelayAddr 的 host；
// 端口来自 HelloOK.TCPDataPort / HelloOK.UDPDataPort。
// 回退：若 HelloOK 未提供端口（0，旧 relay），尝试从本 ini 的
// cfg.Relay.TCPListen / cfg.Relay.UDPListen 取端口。
```

**推导失败不退出 `Run`**（阶段 3 三模式统一后的行为，阶段 4 以此为准）：

- 推导失败只记 `WARN`（`event=data_addr`，agent 侧还会记 `detail`），`Run` 继续运行，
  其它服务/协议不受影响；
- 真正需要该数据面的那一次配对会在 dial 阶段失败：关闭本地连接/会话、记 `WARN`
  （`event=data_open` 或 `upstream_dial_failed`），不影响 `Run` 的其它配对；
- 理由：`Run` 返回错误会让整个进程退出，而"某个协议的某个端口没下发"是可以用
  "放弃本次配对 + 清晰日志"处理的可恢复问题；把进程存活与单次配对解耦，
  与契约 §2.4 第 8 条（本地目标连接失败只影响本次配对）保持一致；
- agent 侧只在 `udp_mode=native` 时才推导 UDP 数据面地址；`tcp_tunnel` 复用
  `tcpDataAddr`，不因 relay 未下发 `udp_data_port` 而告警。

实现建议：在各自包里写一个 `dataAddr(host, port int) (string, error)` 小函数
（代码重复可接受，因为不能新增公共包）；测试覆盖 IPv4/IPv6/域名三种 relay 地址。

### 4.6 UDP 数据面承载模式（`udp_mode`，必须实现两种）

relay 启动时确定模式并在 `HelloOK`、`ServiceRegistered`（agent）、`ServiceList`（client）中下发：

- `native`：`cfg.Relay.UDPListen != ""`，UDP 数据面走原生 UDP；
- `tcp_tunnel`：`cfg.Relay.UDPListen == ""`，UDP 数据面复用 `tcp_listen`。

**relay**：

1. `native`：按 §2.3 的 UDPDataListener 流程处理（`UDPTable` + Bind 数据报 + `MsgBindOK`）。
   阶段 6C 补充的丢包语义（详见 `docs/protocol.md` §5.2.1）：
   - 半开状态收到**同一端点**重复的 `MsgBind` → 幂等，**再次回** `MsgBindOK`
     （令牌只校验不消费），让 BindOK 丢包的 agent/client 能靠重发完成报到；
   - `Full` 之后，来自已知端点、且能解析为**同一配对键** `MsgBind` 帧的数据报 → **丢弃**
     （不得转发，否则会污染对端数据流），只记 debug 日志；实现上只嗅探 5 字节帧头，
     其它数据报仍按源地址原样转发、不做内容解析。
2. `tcp_tunnel`：`tcp_listen` 的 accept 循环读到 `MsgBind.Carrier == "udp-over-tcp"` 后，
   校验该首帧 role/令牌，然后把连接转为 UDP 隧道帧解析（`protocol.ReadUDPTunnelFrame`）：
   - 首帧对应"第一个会话"，校验通过后登记并回控制帧（`session=""` + `MsgBindOK`）；
   - 后续某个会话首次出现：`session=<配对键>`，payload 是该会话的 `MsgBind` 帧；relay 校验
     令牌后登记会话（半开）；
   - 两侧同会话都登记 → 标记 Full 并 `token.Consume`；此后 `session=<配对键>` 的数据帧
     原样转发到对端承载连接；
   - 单会话错误用控制帧（`session=""` + `MsgError`）回报，不关闭承载连接；
   - 承载连接断开 → 删除其上全部会话；半开用 `pair_timeout`、空闲用 `udp_session_timeout`；
   - 写侧必须用**单 writer goroutine + 有界发送队列**（阶段 6C，`internal/tunnelqueue`）：
     读循环只入队、绝不因慢对端阻塞；整帧单次 `Write` 写出；数据帧队列上限 1024 帧 /
     8 MiB、控制帧（`MsgBindOK`/`MsgError`）走独立的优先队列（上限 256 帧），保证会话级
     控制帧不被数据洪泛饿死；队列满时丢弃并计数（`event=tunnel_drop`，限速 WARN）；
     关闭顺序：停止入队 → 关闭队列 → writer 排空（上限 2s）→ 关连接。
3. 同一配对键同时只归一个会话；重复 `MsgBind` 视为幂等（已完成会话不改变状态）。

**agent / client**：

1. 从 `HelloOK`/`ServiceRegistered`/`ServiceList` 读取 `udp_mode`（缺失/未知回退 `native` + WARN）。
2. `native`：为每个 UDP 配对 `net.DialUDP` 到 `udp_listen`（host 来自 relay_addr，端口来自
   `HelloOK.UDPDataPort`），发 `MsgBind` 数据报，`pair_timeout` 内等 `MsgBindOK`；
   阶段 6C 起做丢包重传：每约 **300ms** 未收到应答就重发报到帧，最多重发 **3** 次；
   收到 `MsgBindOK` **或首个业务数据报**即视为就绪；`pair_timeout` 到期后放弃该会话并清理。
3. `tcp_tunnel`：维护**一条**到 `tcp_listen` 的承载连接（建议懒创建、按需重建）：
   - 该连接的第一个 UDP 会话：首帧 `MsgBind{carrier:"udp-over-tcp", key, token, role}`；
   - 之后的会话：发 `session=<配对键>` + payload 为 `MsgBind` 帧的隧道帧；
   - 数据：`WriteUDPTunnelFrame(session=key, payload=数据报)`；读循环按 `session` 分发到
     本地 UDP 会话（`ReadUDPTunnelFrame`）；
   - 写侧同样是**单 writer goroutine + 有界发送队列**（`internal/tunnelqueue`）：业务
     goroutine 只入队、不因慢 relay 阻塞；控制帧优先；队列满时丢弃并计数
     （`event=tunnel_drop`）；**入队即复制 payload**，调用方可复用读缓冲；
   - 等待 `MsgBindOK` 控制帧（`session=""`）确认承载连接就绪，超时 `pair_timeout`；
   - 收到 `session=""` + `MsgError` 时只关闭对应会话，不关闭承载连接。
4. 两种模式下本地 UDP 会话表都要清理：agent 用 `cfg.Server.UDPSessionTimeout`；
   client 用 `cfg.Client.UDPSessionTimeout`（`[client] udp_session_timeout`，默认 60s；
   阶段 4 新增该字段，取代阶段 2/3 的包内常量 60s）。

### 4.7 错误处理与日志

- 连接级错误：记 WARN（`err` key）+ 关闭；不得让进程退出。
- 启动级错误：返回 error 让 `main` 退出。
- 日志事件名遵循 `docs/architecture.md` §6；必须覆盖连接建立、注册、通知、配对、断开、
  超时清理、踢出。
- **不得记录 `pairing_token` 明文**（日志里最多记前后 4 字符或摘要）。

## 5. 阶段 2 交付与自检

每个子代理在自己的目录内交付：

1. 完整实现（可拆多个 `.go` 文件，`Run` 保留在包根文件或 `run.go`）。
2. 包内 `_test.go`：覆盖协议流程的关键路径 + 清理（不要求端到端，端到端归阶段 3）。
3. 自检必须全绿（在仓库根执行）：

```bash
gofmt -l .            # 必须无输出
go vet ./...          # 必须通过
go build ./...        # 必须通过
go test ./...         # 必须全绿
go test -race ./internal/...   # 必须全绿
```

4. 若发现冻结包确实缺少必要能力：**不要改冻结包**，在最终报告中列出契约变更请求
   （目标文件、符号、建议签名、理由、对另外两个模式的影响）。
5. 不要新增第三方依赖；Windows 服务已由 `golang.org/x/sys` 完成。
