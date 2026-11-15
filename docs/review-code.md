# 阶段 8A：v0.3.0 全面代码审查报告

> **审查对象**：`E:\BizWorks\pairlink`，HEAD `df3b26e`（tag `v0.3.0` 已推送）。
> **审查方式**：只读审查（本文件是唯一写入物）。逐行通读 `internal/**` 全部 128 个 Go 文件、
> `test/**`、`deploy/**`、`examples/**` 与 `task.md` / `task2.txt` / `README.md` / `docs/*.md`；
> 实跑 `gofmt` / `go vet` / `go build` / `go test` / `go test -race` / e2e `-count=2`；
> 对可疑点写了两个最小复现程序（`E:\TEMP\review-patch`，未向仓库添加任何文件）。
> **审查问题**：能不能把当前 HEAD 作为可发布的 v0.3.0 交给陌生团队维护？

> **阶段 8C 修复状态（v0.3.1）**：P0-1/SEC-D1、P1-1、P1-2 已修复；P2-1/P2-2/P2-3/P2-6/P2-7
> 已修复，P2-8 部分修复（连接/令牌/在途配额 + agent 并发上限，仍无按 client 的令牌签发速率）；
> P2-4（Windows ACL/目录 fsync）与 P2-5（锁内 IO）**未修**，原因见 README §15 / 交付报告；
> P3-1/P3-4/P3-5/P3-6/P3-8/P3-10 已顺手修复，其余 P3 保留。逐项证据与回归数字见 README §15、
> `docs/architecture.md` §7.10（完整安全审查报告含攻击步骤，不随仓库发布）。

---

## 1. 结论摘要

| 项 | 结论 |
| --- | --- |
| **总体质量评级** | **B**（工程化程度高、边界清晰、测试体系完整；但存在 1 个可被未认证远程触发的可用性缺陷，以及 2 处"文档承诺 ≠ 部署/默认状态下实际可用"的集成缺陷） |
| **是否可发布** | **暂不建议**直接以当前 HEAD 发布 v0.3.0（详见 §7）。修完 P0-1 + P1-1 + P1-2（预估 0.5–1 人日，改动集中在 4 个文件）后，可作为 **v0.3.1** 发布 |
| **P0（阻断）** | **1** 条 |
| **P1（严重）** | **2** 条 |
| **P2（一般）** | **8** 条 |
| **P3（建议）** | **10** 条（合并同类后列出） |
| **自检结果** | gofmt / vet / build 全干净；17 个包 `-count=1`、`-race` 全绿；e2e `-count=2` 无 flake；未发现数据竞争 |
| **最大优点** | 责任划分（控制面/数据面/三模式包边界）、令牌"校验-消费"语义、关停顺序、e2e 证据强度、文档诚实度都在同龄项目均值之上（见 §6） |

**一句话**：代码质量足以交付维护，但 v0.3.0 的"管理页可写"这个卖点，在**按 README/deploy 部署**时（systemd + 容器）和**按默认页面状态**使用时都跑不通，另外数据面报到帧有一个低成本的远程内存放大口子——这三点必须先修。

---

## 2. 发现清单

> 级别：**P0 阻断** / **P1 严重** / **P2 一般** / **P3 建议**。
> 位置行号基于 HEAD `df3b26e`（已逐条核对）。

| ID | 级别 | 位置 | 问题 | 影响 | 证据 | 修复建议 |
| --- | --- | --- | --- | --- | --- | --- |
| **P0-1** | **P0** | `internal/protocol/protocol.go:184-195`；触发入口 `internal/relay/udp_data.go:91`、`:178`、`internal/relay/tcp_data.go:131` | `ReadFrame` 只校验 `n <= MaxPayload(1 MiB)` 就 `make([]byte, n)`，**不校验对端是否真的会发这么多字节**。UDP 数据面任何未登记端点发来的数据报都会走 `protocol.ParseFrame` → 分配 1 MiB 后才发现数据不够 | 未认证远程可低成本耗尽 relay 内存；UDP 侧为每包 1 MiB 分配（放大 ≈20 万倍），TCP 侧每连接 1 MiB 且被 `pair_timeout` 持有最长 10s（1 万连接 ≈ 10 GiB）。这直接违反 `docs/protocol.md:44-47` "超限立即返回 `ErrPayloadTooLarge`，**不分配对应内存**，防内存放大" 的承诺 | 5 字节数据报（`0x0C` + 声明 `0x00100000`）→ 每次实测分配 **1024.4 KiB**（200 次累计 200.1 MiB，见 §8 复现输出） | ① 数据面报到帧改用已经存在但**生产代码从未使用**的 `protocol.ReadFrameWithLimit(conn, 64<<10)`（Bind 帧实际只有 ~200 字节；`internal/protocol/protocol.go:175` 的该 API 目前只有单测在用）；② UDP 侧在 `ParseFrame` 前加"声明长度 ≤ 实际收到的字节数"预检，或新增 `ParseFrameLimit(data, limit)` 把上限设为 `len(data)`；③ 控制面同理降到 64 KiB（服务列表上限远小于此） |
| **P1-1** | **P1** | `deploy/systemd/pairlink-relay.service:46`、`pairlink-agent.service:39`、`pairlink-client.service:39`；`deploy/docker-compose.yml:26,41,69,81`；代码 `internal/config/patch.go:321-352` | 三个 systemd unit 全部 `ProtectSystem=strict` 且**没有 `ReadWritePaths=/etc/pairlink`**（`deploy/systemd/README.md:76` 还明确写"`/etc/pairlink` 仍然**可读**"）；compose 里 ini 是 `:ro` 挂载 + `read_only: true` 根文件系统。而 v0.3.0 的写能力是在**同目录建临时文件 + rename** | 按 README §8 部署（systemd）或按 compose 部署时，管理页所有写操作**必然失败**（`EROFS: read-only file system`），页面上只显示"写回 ini 失败（运行时未改变）"。即使以 root 运行也照样失败（`/etc` 在服务命名空间内是只读挂载） | 单元文件 `ProtectSystem=strict` 无 `ReadWritePaths`；`install -d -m 0750 -o root -g pairlink` + `install -m 0640`（`deploy/linux/common.sh:229,244`）也决定非 root 用户不可写 | ① 三个 unit 增加 `ReadWritePaths=/etc/pairlink`；② 容器方案改为把 ini 所在目录挂成可写卷（或文档明确"容器内不支持页面写回，请改 ini + 重建容器"）；③ `deploy/README.md` / `deploy/systemd/README.md` / README §6 增加一行"页面写回要求 ini 目录对服务用户可写" |
| **P1-2** | **P1** | `internal/admin/layout.go:93` + `:158-163`；表单在 `internal/relay/admin_config.go:94`、`internal/agent/adminpage.go:146-148,160-164`、`internal/client/admin.go:115-121,134-139` | 所有页面默认注入 `<meta http-equiv="refresh" content="1">`（`autoRefresh` 默认 true），而同一页面上就有需要用户键入的文本框 | 浏览器每秒整页重载，**正在输入的内容每秒被清空**：relay 的五个超时参数、agent 的 local、client 的监听地址在默认状态下基本无法输入。必须先发现顶栏"暂停自动刷新"链接（`?refresh=0`）才能用。e2e 全部直接 POST + `/?refresh=0`，所以测试全绿而人工不可用 | 实跑 relay 后抓取 `/relay`：`<meta http-equiv="refresh" content="1">`，同页 5 个 `<form>` | 可写（`writable`）时默认 `RefreshOn=false`，或仅对无表单页面启用刷新；保留顶栏开关；README §6/§10.7 明确说明"编辑前请先暂停自动刷新" |
| **P2-1** | P2 | `internal/relay/udp_data.go:119-126` | 原生 UDP 令牌校验失败按 `Warn` 记录，且发生在任何鉴权之前、单读循环内 | 未认证攻击者每包一条 WARN（≈200 B/包，放大 40 倍）刷 journald / 磁盘；同时把单线程读循环拖慢，影响正常 UDP 转发 | 代码路径：`handleDatagram` → `tokens.Validate` 失败 → `log.Warn("UDP 报到令牌校验失败", ...)` | 限速（每源/每秒最多一条）或降为 `Debug` + 计数器，由管理页暴露计数 |
| **P2-2** | P2 | `internal/relay/tunnel.go:76-79`（accept 即 `Info` 日志）、`:89-97`（每连接一 goroutine） | 隧道端口无连接数上限，且每次 accept 打一条 `Info` | 未认证连接洪水可刷日志 + 吃 fd/goroutine（每个半开连接最长占用 `controlHelloTimeout=10s`） | `acceptLoop` 仅 `wg.Add(1)`，无 `maxConns`/限速 | 加连接数上限（如 4096）与 accept 日志限速；超限直接 `Close` 并计数 |
| **P2-3** | P2 | `docs/traceability.md:1`（"v0.2.0 现状"）、`:8`（"行号为**当前工作区**真实行号"） | 矩阵声称锚点是当前真实行号，实测 231 个测试锚点中 **38 个漂移**（多数 +2..+5，`internal/client/mapping_admin_test.go` 两条 +16/+21，`internal/relay/admin_config_test.go` 两条 -1）；564 条链接全部仍可解析、未越界 | 维护者点进去落在函数尾部/别的用例上，削弱"可交给陌生团队"的可信度；标题时点停在 v0.2.0 | 脚本比对：564 条链接 0 越界；38/231 测试锚点 `func` 起始行不符（如 `test/binary_test.go` 文档 285 / 实际 288、`mapping_admin_test.go` +21） | 用脚本从 `func Test...` 重新生成锚点；标题与 `取证基线` 改为 v0.3.0 |
| **P2-4** | P2 | `internal/config/patch.go:123`（`mode: info.Mode().Perm()`）、`:340`（`tmp.Chmod(mode)`）、`:336-351`（无目录 fsync） | "保留权限位"只在 POSIX 语义下成立：Windows 上 `FileMode.Perm()` 只有只读位有意义，**ACL/所有者不保留**；rename 后文件属主变成写进程，且未 `fsync` 目录 | Windows 部署下 ini 里的 `auth_token`/`token` 可能因继承目录 ACL 而被更宽的账户读到（安全回归）；掉电后 rename 可能丢失（配置回退） | 代码路径：临时文件在同目录创建，只 `Chmod`，无 `ReplaceFile`/ACL 复制；`write()` 只 `tmp.Sync()` | Windows 用 `MoveFileEx`+显式 ACL 复制（或文档写明需自行收紧目录 ACL）；写完后 `fsync` 目录 |
| **P2-5** | P2 | `internal/agent/tunnel.go:70-101`（持 `m.mu` 完成 `dialTCP`+`writeFirstFrame`）；`internal/client/tunnel.go:343-378`（持 `carrierDialMu` 完成 `dialTCPData`+`WriteFrame`） | 在锁内做网络 IO，且**与本项目自己的约定冲突**：`docs/architecture.md:275`"不在锁内做 IO"、`internal/client/client.go:163`"所有 IO（Close/Write/Dial）都在锁外进行" | relay 慢/不可达时，一次 UDP 会话建立会阻塞同进程内**其它所有** UDP 会话建立（最长 `dial_timeout=10s`）；此类 bug 在压测/弱网下才暴露，很难排 | 代码：`m.mu.Lock()` 后紧跟 `m.a.dialTCP(...)`；`c.carrierDialMu.Lock(); defer Unlock()` 后 `c.dialTCPData()` | 改成"先拨号、后加锁双重检查再登记"（已有的 `carrierDialMu` 只保护指针替换）；失败时关闭新建连接 |
| **P2-6** | P2 | `internal/protocol/protocol.go:345-349` vs `docs/protocol.md:72-73` | 文档写 `sess_len` 取值 `0..129`（`MaxSessionIDLen`），但读侧只校验 `sessionLen > n-1`，实际接受 0..255；写侧 `WriteUDPTunnelFrame:307` 才拒绝 >129 | 契约与实现对不上；255 字节会话标识虽会被后续 `ParsePairingKey` 拒绝（不会越界/放大），但"格式校验"这一层被绕过 | 代码：`if sessionLen > n-1 { ... }`，无 `sessionLen > MaxSessionIDLen` | 读侧补 `sessionLen > MaxSessionIDLen` 拒绝，并加一个边界用例 |
| **P2-7** | P2 | `internal/relay/carrier.go:189-193`（`owns` 为真直接当数据帧转发）vs `internal/relay/udp_data.go:71-77`（原生 UDP 的 `bind_dup_drop`） | 承载连接上，已登记会话再发一个 `MsgBind`（`session=<key>`+Bind 帧）会被**原样转发给对端**当业务数据；原生 UDP 的同类情况是**丢弃** | 两种承载对"重复报到"的处理不一致；当前三端都不重发，属于潜在污染（一旦将来给承载连接加重发/重试，对端本地目标会收到一个 PairLink 帧） | 代码对比：`carrier.go` 先 `owns` 再 `deliverCarrierData`，没有 5 字节帧头嗅探 | 在 `owns` 分支前复用 `parseBindKey`（或等价嗅探）：同一 key 的 Bind 帧丢弃并记 `bind_dup_drop` |
| **P2-8** | P2 | `internal/relay/tunnel.go:498-501`（`too_many_pending` 只比 `pairs.Len()`）、`internal/token/token.go:99`（令牌表无上限）、`internal/agent/control.go:342-346`（每个 `MsgIncoming` 起 goroutine） | `too_many_pending` 只约束**数据面占位**，而 `ConnectRequest` 本身不建占位：持 token 的 client 可无限签发令牌并把 `MsgIncoming` 灌给 agent | 认证 peer（或泄露的 client token）可把 relay 令牌表与 agent 的 goroutine/连接数推到无界；错误码 `too_many_pending` 名不副实 | 代码：`pairs.Len()` 的条目只在 `PairingTable.Claim`（数据面）产生；`tokens.Issue` 无配额；agent `controlLoop` 每通知一个 goroutine 无上限 | 在 `TokenStore` 加容量上限或按 client 限流（例如每 client 在途请求 ≤ 64），超限回 `too_many_pending`；agent 侧加并发会话上限（超限直接拒绝并记 WARN） |
| P3-1 | P3 | `internal/protocol/protocol.go:175` | `ReadFrameWithLimit` 生产代码零使用（仅 `protocol_test.go` 用），实际是一条"写了但没接上"的防护 | 死代码，且正是 P0-1 需要的那个 API | `grep ReadFrameWithLimit` 只有定义+单测 | 随 P0-1 一起接入生产路径 |
| P3-2 | P3 | `internal/agent/agent.go:287-293` | agent 的 `pairTimeout()` 读 `[relay] pair_timeout`，而 agent 的 ini 通常没有 `[relay]` 段 → 恒为 10s | relay 被管理页改成 3s/30s 后 agent 侧等待 BindOK 的超时不会跟随（日志里会出现"看起来不一致"的超时） | 代码注释自己承认"等价于默认值" | 在 `HelloOK`/`ServiceRegistered` 里下发 relay 当前 `pair_timeout`，或 agent ini 增独立键 |
| P3-3 | P3 | `internal/relay/server.go:305-309` | `runTickers` 的 `pairInterval` 在启动时按当时的 `pair_timeout/2` 固定 | 运行期把 `pair_timeout` 改到远小于初始值（如 10s→200ms）时，扫描周期仍是 5s，占位清理滞后 | 代码：`pairInterval := s.pairTimeout()/2` 只在启动执行一次 | ticker 周期取固定小值（如 100ms），或改值时重建 ticker |
| P3-4 | P3 | `internal/protocol/messages.go:63` vs `docs/protocol.md:146` | `udp_mode` 在 `HelloOK` 的 JSON tag 是 `omitempty`，文档标"必填/是" | 旧 relay 或异常 relay 不发该字段时，文档契约与线格式不一致（新端会 WARN 回退 native） | tag `json:"udp_mode,omitempty"` | 文档改为"是（relay v0.3.0+ 必发，缺省按 native 回退）" |
| P3-5 | P3 | `deploy/systemd/pairlink-agent.service:8` | 注释仍写"agent 控制连接断开后进程会退出…（第一版无内置重连）" | 与 v0.2.0 起的内置重连（`reconnect = true` 默认）直接矛盾，会让运维误判 `Restart=on-failure` 的作用 | 同一文件 `Restart=on-failure` + README §10.1 | 改注释为"默认进程内重连；`reconnect = false` 时才依赖 Restart" |
| P3-6 | P3 | `docs/protocol.md:344-347` vs `internal/agent/service_admin.go:188-232` | 文档说 `MsgUnregisterService` 用于"删除/**修改**服务"，代码只在删除时发注销；修改走"重新注册=更新" | 契约描述过宽，读文档的人会以为修改也是先注销 | `saveService` 只 `PatchValue`+`registerServiceOn`；`deleteService` 才 `unregisterServiceOn` | 文档改为"删除时用；修改复用重复注册（relay 视为 update）" |
| P3-7 | P3 | `internal/client/admin.go:114`、`internal/client/mapping_admin.go:94-96` | client 管理页拒绝新增/修改/删除 `service.default` 映射，但 `[client] service.default` 在 ini 里合法且会被加载 | 用户若在 ini 里写了 `service.default`，页面上既不能改也不能删（只能改 ini + 重启），行为不一致 | 页面条件 `m.ServiceID != config.DefaultServiceID`；`validateMappingForm` 显式拒绝 `default` | 要么统一禁止（Load 报错，如 `[server]` 的 `service.default`），要么允许编辑 |
| P3-8 | P3 | `internal/relay/tcp_data.go:205-215` | 完成配对时若 `w.claim()` 返回 false（等待方已被踢出/agent 下线取消），只 `closePairPeers`，**不 `pairs.Remove(key)`** | 该键在最长 `pair_timeout` 内仍返回 `already_paired`（占位靠 `Sweep` 兜底），期间同一配对键的重试被拒；不影响正确性，但清理不"及时" | 代码：该分支无 `s.pairs.Remove(key)`，而超时分支有 | 加一行 `s.pairs.Remove(key)`（幂等） |
| P3-9 | P3 | `internal/config/patch.go:129`（`f.crlf = strings.Contains(text, "\r\n")`） | 混合换行文件（部分 LF、部分 CRLF）会被整体重写成 CRLF | 手写/跨编辑器编辑过的 ini 会被"整篇改换行"，与"保留换行风格"的承诺有偏差（diff 噪音） | 代码：只要出现一处 `\r\n` 就全文件 CRLF | 逐行记录原换行符，或仅在"全部行都是 CRLF"时按 CRLF |
| P3-10 | P3 | `internal/relay/admin_config.go:139-143` | "已写回 ini，但更新运行时失败"分支实际不可达（`ParsePositiveDuration` 已保证 `>0`，而 `Set` 只在 `<=0` 时报错） | 死分支，给"部分成功"留了一个永远不会被验证的路径 | `SetTimeout`/`SetTTL` 的唯一错误条件是 `<=0` | 保留但注释说明不可达；或让 `Set` 无返回值以简化 |

---

## 3. 重点领域深评

### 3.1 协议一致性 —— 结论：**线格式与字段名一致；有 3 处"文档承诺 > 代码保证"**

**依据**

1. **帧格式/消息表/字段名逐项核对通过**：`protocol.md` §2 的 `type|len|payload(1 MiB)` 与 `protocol.go:143-196` 一致；§3 的 20 个类型（0x01–0x14）与 `messages.go` 常量一一对应；§4 的每个 JSON 字段名与 struct tag 全等（含 `client_conn_id`、`udp_mode`、`carrier`）。
2. **0x13/0x14 实际落地**：`UnregisterService`/`ServiceUnregistered` 在 relay `tunnel.go:386-424` 与 agent `control.go:370-383, service_admin.go:109-123` 双向闭环，错误码复用 `bad_request`，未提升 `ProtocolVersion` ✓。
3. **TCP 数据面"成功前零字节"**：`handleTCPBind` 先到者路径只 `select` 等待，不写任何字节；`finishPair` 不回 BindOK ✓（与 §4.9 一致）。
4. **不一致点**：
   - P0-1：§2 承诺"超限不分配内存"，实际对"声明合法但数据未到"的帧仍按声明长度分配；
   - P2-6：§2.1 `sess_len 0..129`，读侧接受 0..255；
   - P3-4：§4.2 `udp_mode` 标"是"，tag 是 `omitempty`。

### 3.2 TCP 配对状态机 —— 结论：**语义正确，`Consume` 恰好一次，`Remove` 全覆盖；1 处清理不够及时**

**依据**（`internal/pairing/pairing.go` + `internal/relay/tcp_data.go`）

| 分支 | 行为 | 连接所有权 | 结论 |
| --- | --- | --- | --- |
| 首次 `Claim` → `ErrWaiting` | 占位、静默等待（`timer / w.done / ctx`），**零字节** | 自己持有 | ✓ |
| 后到 `Claim` → `nil` | `w.claim()` 成功后由**后到者**独占两条连接并 `finishPair` | 后到者接管 | ✓ |
| `ErrDuplicateSide` | 关闭新连接、不动已有半占位 | — | ✓（符合 §5.1，等待方不受影响） |
| `ErrAlreadyPaired` | 记 `pair_replay` 后关闭 | — | ✓（`completed` 条目保留一个 `pair_timeout` 窗口） |
| `ErrTimeout` | `Claim` 内删除条目并 `fireExpire` 关闭已到的一侧 | — | ✓ |
| 等待期被踢/agent 下线 | `w.cancel()`→ 等待方关闭自己 | — | ✓，但 P3-8：完成方不 `Remove` |
| 超时与对端到达竞态 | `outcome()`→`cancel()`→再 `outcome()` 三段处理，两边都失败或都成功 | — | ✓（推演了 4 种交错，无"一方转发另一方已关"） |
| deadline 清除 | `finishPair` 对两端 `SetDeadline(time.Time{})` | — | ✓ |
| `token.Consume` | **只在 `finishPair` 调用**；`Claim` 返回 `nil` 每个键最多一次 | — | ✓ 恰好一次 |
| `PairingTable.Remove` | `finishPair` 正常/异常路径、超时路径、踢出、agentGone、Sweep 均覆盖 | — | ✓（唯一缺口是 P3-8，由 Sweep 兜底） |

### 3.3 UDP 两条承载 —— 结论：**语义与文档一致，payload 复制正确；两处需修（P0-1、P2-7）**

**原生 UDP**（`relay/udp_data.go` + `registry/udp_table.go`）
- 源地址反查：`LookupByEndpoint` 先于一切解析 ✓；半开时 `peer.IsValid()==false` 一律丢弃 ✓；
- 只有 `Full` 转发 ✓；半开用 `CreatedAt+pair_timeout`、Full 用 `LastSeen+udp_session_timeout` 两套清理 ✓；
- 令牌：半开只 `Validate`，`full` 时才 `Consume` ✓；重复 Bind 幂等再回 `BindOK` ✓；Full 后重复 Bind 帧丢弃并记 `bind_dup_drop` ✓；
- 单读循环、无 per-packet goroutine ✓（`loop()` 内同步 `handleDatagram`）；
- **缺陷**：未登记端点的数据报必经 `ParseFrame` → P0-1 的 1 MiB 分配；令牌失败 WARN 无节流（P2-1）。

**UDP over TCP**（`relay/carrier.go` + `tunnelqueue` + agent/client 两侧）
- 承载首帧 `MsgBind{carrier}` → 校验令牌 → 回 `session=""` 的 `BindOK` → 进入帧解析 ✓；
- 会话登记/`half→full→Consume`、单会话错误用控制帧 `detail=<配对键>` 隔离、不杀承载连接 ✓；
- 写侧统一"单 writer + 有界队列（数据 1024 帧/8 MiB、控制 256 帧）+ 整帧单次 `Write` + 队列满丢弃计数" ✓；关闭顺序"停止入队 → 关队列 → writer 排空/超时强关 → 删会话" ✓；
- payload 复制：`Queue.SendData` 内 `append([]byte(nil), payload...)`，读循环复用缓冲不会被踩 ✓（三端 readLoop 传的都是切片的都是"读缓冲"，靠这一层复制兜住）；
- 承载断开清理：`removeCarrier`/`removeAgent` 删会话，agent/client 侧 `fail()/close()` 关闭其上全部本地会话 ✓；
- **缺陷**：P2-7（重复 Bind 被转发）、P2-6（session 长度上限）。

### 3.4 管理页写路径 —— 结论：**顺序与回滚设计正确，但部署形态与默认交互不可用（P1-1、P1-2）**

| 模式 | 承诺（contract §3.10） | 代码 | 结论 |
| --- | --- | --- | --- |
| relay 五参数 | 校验 → 写 ini → 改运行时（写盘失败则不改） | `relay/admin_config.go:123-146` 严格按此顺序；`Set` 只在 `<=0` 失败（已校验） | ✓（P3-10 死分支） |
| agent 服务 | 改内存表 → 向 relay 注册/注销并等确认（3s）→ 成功才写 ini → 失败回滚 | `agent/service_admin.go:145-185`/`:188-232`/`:235-295`；`rollbackService:300-316` 尽力恢复内存+注册 | ✓，唯一不可逆窗口是"ini 写成功但进程崩溃"（可接受） |
| agent 主服务 | 两行（`protocol`+`local`）要么都成、要么都回滚 | `saveMainService:281-289`：先写 `local` 再写 `protocol`，失败 `revert()` 把两行都写回旧值 | ✓（回滚本身也可能失败，已 WARN） |
| client 映射 | 监听成功才落盘；失败回滚旧监听 | `mapping_admin.go:127-158`（新增）、`:161-227`（改地址：先开新后关旧，失败重开旧）、`:230-253`（仅协议变化走热切换）、`:256-308`（删除→失败重开） | ✓，与"先关后开"相比更安全；P3：回滚里 `serveListener` 在 `tasks` 已关闭时会静默不启动（只在关停窗口） |

**写路径的格式保持/原子性**（`internal/config/patch.go`，我用独立程序实跑验证，见 §8）：
- 重复键全部替换 ✓、重复段全部覆盖 ✓、CRLF+BOM+无结尾换行原样保留 ✓、空文件新建段 ✓、注释/空行/对齐保留 ✓；
- 注入拒绝：值含 `\r`/`\n`、键含 `=`、段名含 `[]` 全部报错 ✓；
- 同值重复 Patch 幂等（不产生 diff）✓；同目录临时文件 + `Rename` ✓；
- 缺陷：P2-4（Windows ACL/目录 fsync）、P3-9（混合换行被统一成 CRLF）。

### 3.5 并发与泄漏 —— 结论：**没有数据竞争、没有 goroutine 泄漏；两处锁内 IO**

**依据**
- `-race ./...` 全绿（17 包，无 `DATA RACE`/`panic`）；e2e `-count=2` 无 flake。
- 共享状态全部有锁或原子量：`ServiceRegistry`(RWMutex)、`AgentRegistry`、`PairingTable`、`TokenStore`、`UDPTable`、`carrierTable`、relay 的 `waits/active/agentConns/dataConns`、agent 的 `sessions/svcStore(s)/ctrl/addr`、client 的 `neg/svc/tcp/udp/listener/mapping/carrier`、admin 的 `csrfTokens`/`pages`。逐包核对了锁的获取顺序，**未发现锁序反转**。
- **锁内 IO**：agent `carrierManager.openSession`（`m.mu` 跨 `dialTCP`+`WriteFrame`）、client `acquireCarrier`（`carrierDialMu` 跨 `dialTCPData`+`WriteFrame`）→ P2-5，且与 `architecture.md:275` 的规则冲突。
- `wg.Add` 竞态：client 用 `taskGroup` 明确规避；agent 的 `a.wg.Add` 只发生在 `run()` 单线程阶段、`controlLoop`（其 goroutine 在 `supervise` 内、`shutdown` 之后才 `Wait`）与已计数的会话 goroutine 中 —— 推演未发现"零计数 + Wait 并发 Add"的组合，**安全**。
- ticker：relay `runTickers` 4 个 ticker 全部 `defer Stop()`；agent `sweepLoop`、client `sweepLoop`、两侧 `heartbeatLoop` 均 `defer Stop()` 且受 ctx 约束 ✓。
- 关停顺序：relay（关监听 → 关数据连接 → 关管理页 → cancel → `wg.Wait`）、agent（cancel → 关控制 → 关会话/承载 → `wg.Wait` → 关管理页）、client（cancel → `tasks.Close` → 关监听 → 关控制 → 关会话 → `Wait` → 关管理页）与 `architecture.md` §5 一致 ✓；`Close` 全部幂等（`sync.Once`/`closed` 标志）。
- e2e `TestE2EGoroutineNoLeak` 用 pprof 断言"运行中无 `forward.Bidirectional` 残留、退出后无 `internal/relay|agent|client|forward` 栈帧"——这是**强证据**，不是"只看计数"。

### 3.6 配置往返一致性 —— 结论：**写回再读回一致，边界正确；1 处换行风格偏差**

**依据**（实跑，见 §8 输出）
- `PatchValue("relay","pair_timeout","42s")` 后 `Load` → `42s` ✓；`[server] local`/`protocol`/`service.<id>` 写回后 `Load` 得到同一个规范化地址 ✓；
- `service.<ID>` 写盘值 `tcp:127.0.0.1:8080` 经 `parseServerServiceValue` 读回 → `tcp` + `127.0.0.1:8080`，与写前完全相同 ✓；IPv6 `tcp:[::1]:8080` 同样往返一致 ✓；
- 重复段合并、重复键后者覆盖、键名大小写不敏感（`keyLineEq` 用 `EqualFold`）✓；行内 `;`/`#` 属于值本身（`parseINI:79` 注释、`examples_test.assertNoInlineComment` 强制模板不写行内注释）✓；
- 只有 `"0"`/负值/非法 duration 被 `Load` 拒绝（`config.go:339-341`），管理页 `ParsePositiveDuration` 同规则 ✓（**写回的每个值都能被 `Load` 原样接受**，这是关键不变量，成立）。

---

## 4. 测试盲区清单（按风险排序）

| # | 盲区 | 风险 | 现状 | 建议 |
| --- | --- | --- | --- | --- |
| 1 | **报文字段长度 vs 实际可用字节**（P0-1）：无任何用例发"声明 1 MiB 的 5 字节帧" | 高 | `protocol_test.go` 只测"声明超 MaxPayload 拒绝"（`TestReadFrameWithLimit`），没测"合法声明但数据不足"的分配行为 | 加 `Benchmark/TestReadFrameTruncatedAlloc` + data-plane 上限用例 |
| 2 | **部署形态下的写回**（P1-1）：无 systemd/容器内的写操作验证 | 高 | 写路径 e2e 全部在临时目录 + 进程内运行 | WSL2 上装 unit 跑一次页面写；或至少加"只读目录 → 写失败 → 运行时不变"的 e2e |
| 3 | **人工交互路径**（P1-2）：所有写用例直接 POST，不解析页面表单，不感知 1s 刷新 | 高 | `e2e_admin_write_test.go` 用 `adminCSRFToken`+`POST` | 加一个"页面包含表单 → 提取 action/hidden 字段 → 提交"的用例；或渲染层断言"可写页面不含 meta refresh" |
| 4 | **Windows 专有路径**：`patch` 的写失败/权限、`-service` 完整生命周期、ACL 保持 | 中 | `TestPatchReadOnlyDir` 在 Windows 直接 `Skip`；SVC 生命周期文档标注"需管理员" | CI 加 Windows 上的只读文件（`attrib +R`/ACL）用例；管理员环境补 SVC 生命周期 |
| 5 | **重复报到/重放的一致性**：承载连接的重复 `MsgBind`（P2-7）、半开 `Claim` 与超时竞态的多轮交错 | 中 | 原生 UDP 有 `bind_dup_drop` 用例；承载侧无 | 补"承载连接重发同 key Bind → 对端不应收到该帧" |
| 6 | **配额/限流**：`too_many_pending` 真实触发、令牌表增长、隧道连接洪水、日志刷屏（P2-1/P2-2/P2-8） | 中 | 无 | 加"pairs 上限"单测（现在只能靠 4096 次循环，成本高，可注入上限常量） |
| 7 | **patch 的 BOM + CRLF + 无结尾换行组合** | 低 | 单测分别覆盖 CRLF、无结尾换行、权限，BOM 无 | 我已手工验证通过（§8 case 1），补成用例防回归 |
| 8 | **临时端口 TOCTOU**（`freeTCPPort` = bind:0→close→reuse） | 低 | README §10.16 已诚实标注 | 长期改为单 listener 复用 |

**已确认"不是盲区"的**（避免误判）：令牌错误/复用/过期、踢出、空闲超时、控制连接断开、UDP 两种承载、并发 20 连接、goroutine/pprof 无残留、examples `Load+Validate`、跨平台交叉编译 —— 都有真实用例且断言较强。

---

## 5. 文档与代码不一致清单（逐条给出正确写法）

| # | 文档位置 | 现在写的 | 应改为 |
| --- | --- | --- | --- |
| 1 | `docs/protocol.md:44-47` | "超限立即返回 `ErrPayloadTooLarge`，**不分配对应内存**，防内存放大" | 加一句限定："仅当声明的 `payload_len` 超过 `MaxPayload` 时才不分配；**声明合法但数据未到时仍会按声明长度分配**（v0.3.0 已知问题，见 P0-1），数据面应改用 `ReadFrameWithLimit` 收紧上限" —— 或直接修代码后保留原文 |
| 2 | `docs/protocol.md:72-73` | `sess_len` 范围 `0..129` | 读侧实现目前接受 `0..255`；修改代码为 `> MaxSessionIDLen` 拒绝后文档保持不变 |
| 3 | `docs/protocol.md:146` | `HelloOK.udp_mode` 必填"是" | "是（relay v0.3.0+ 必发；缺省时对端 WARN 并回退 `native`）"，与 `omitempty` 对齐 |
| 4 | `docs/protocol.md:325-326` | "已完成会话的重复 `MsgBind` 视为幂等" | 补充"承载连接上的重复 Bind 帧目前会被当数据转发给对端（P2-7），原生 UDP 则丢弃" |
| 5 | `docs/protocol.md:344-347` | `MsgUnregisterService` 用于"删除/**修改**服务" | "仅删除时使用；修改通过重复 `MsgRegisterService` 完成（relay 视为 update 并推送 `updated`）" |
| 6 | `docs/traceability.md:1,8` | "v0.2.0 现状"、"行号为当前工作区真实行号" | 更新为 v0.3.0 并重新生成锚点（38/231 已漂移） |
| 7 | `deploy/systemd/pairlink-agent.service:8` | "第一版无内置重连，由 `Restart=on-failure` 拉起" | "默认进程内自动重连（`reconnect = true`）；仅 `reconnect = false` 时依赖 `Restart`" |
| 8 | `deploy/systemd/README.md:76` | "`ProtectSystem=strict` 把整个文件系统挂成只读；`/etc/pairlink` 仍然**可读**" | 补充"管理页写回需要 `ReadWritePaths=/etc/pairlink`，否则写操作返回 EROFS（P1-1）" |
| 9 | `README.md:443`（§6） | "页面默认 1 秒自动刷新，可点右上角暂停/开启" | 补充"**编辑配置前请先暂停自动刷新**，否则输入会被每秒清空" |
| 10 | `README.md:601`（§10.1） | 自动重连已内置（正确） | 与 #7 的 unit 注释矛盾，以 README 为准并修 unit |
| 11 | `docs/contract.md` §3.10 | 写能力边界、顺序、回滚、CSRF、白名单 | **与实现一致**（逐条核对通过），仅需补"要求 ini 目录对服务用户可写" |

---

## 6. 值得保留的优点

1. **控制面/数据面端口与职责分离彻底**：数据面只在建立瞬间读一帧，relay 从不"顺手解析"上层协议；UDP 隧道帧的封装边界也写清了"帧头属于 PairLink 封装，不违反协议无关"。
2. **令牌语义严谨**：只存 SHA-256、`subtle.ConstantTimeCompare`、与配对键绑定、`Consume` 恰好一次、"消费后保留到过期以区分已用"——TCP/UDP 两条数据面共用同一套，没有第二实现。
3. **清理顺序统一且可讲清**：三模式都遵循"关连接 → 等转发 goroutine 返回 → 删表项"，`Close` 全部幂等；`PairingTable` 用单 ticker + 单锁，不用 `time.After` per-pair。
4. **管理页写路径的"事务感"**：三模式各自的"先/后"顺序、回滚、审计事件（`admin_action` / `_denied` / `_failed`）、白名单 + `OnlyPostFields`、一次性 CSRF、回环门禁——这是很多同类项目缺的部分。
5. **ini 补丁工程化到位**：行级替换 + 原子 rename + 进程内串行 + 注入拒绝 + CRLF/BOM/权限保持，且有 18 个单测。
6. **文档诚实度高**：`README §10` 18 条已知限制（含"容器未验证""soak 只跑 2.5 分钟""loopback only"），`docs/traceability.md` 按需求逐条对照——不粉饰，这对"交给陌生团队"极其重要。
7. **测试体系分层清楚**：协议/单元/集成/e2e/真实二进制/soak 分开，e2e 用 pprof 和 ini 正文做"行为级"断言，不是"只跑不验"。
8. **依赖极少**（仅 `golang.org/x/sys`），交叉编译与 `CGO_ENABLED=0` 都照顾到了；`internal/version` 单一版本来源。

---

## 7. 建议的修复顺序（按风险/成本）

| 顺序 | 内容 | 涉及文件 | 预估 |
| --- | --- | --- | --- |
| **1** | **P0-1**：数据面报到帧读上限收紧（`ReadFrameWithLimit(conn, 64<<10)`）+ UDP 侧"声明长度 ≤ 实收字节"预检 + 补用例 | `internal/protocol/protocol.go`（新增 `ParseFrameLimit` 或在调用点预检）、`internal/relay/tcp_data.go:131`、`udp_data.go:91,178` | 0.5 天（含测试） |
| **2** | **P1-1**：三个 unit 加 `ReadWritePaths=/etc/pairlink`；compose 改为可写卷或文档标注不支持；`deploy/README.md`、`deploy/systemd/README.md`、README §6 同步 | `deploy/**`（仅注释/配置，不动代码） | 1–2 小时 |
| **3** | **P1-2**：可写页面默认不自动刷新（或仅无表单页面刷新）；README §6/§10.7 补一句"编辑前先暂停刷新" | `internal/admin/layout.go:93,121-134,158-163` + 3 处表单渲染断言 | 1–2 小时 |
| **4** | **P2 批量**：P2-1 日志限速、P2-2 连接上限、P2-6 会话长度校验、P2-7 重复 Bind 丢弃、P2-5 锁内 IO 改造 | `relay/udp_data.go`、`relay/tunnel.go`、`protocol/protocol.go`、`relay/carrier.go`、`agent/tunnel.go`、`client/tunnel.go` | 0.5–1 天 |
| **5** | **P2-3/P2-4/P2-8**：重新生成 traceability 锚点、patch 的 Windows ACL/目录 fsync、令牌与在途请求配额 | `docs/traceability.md`、`config/patch.go`、`token/token.go`+`relay/tunnel.go` | 1 天 |
| **6** | **P3 批量**（文档类居多）：P3-2/3/4/5/6/7/8/9 + §5 的一致性表逐条改 | 少量代码 + 多份文档 | 0.5 天 |
| **7** | 打 **v0.3.1**（P0/P1 修完即可发；P2 建议同期修完，P3 可进 v0.4.0） | — | — |

---

## 8. 自检命令与真实输出摘要

```
$ go version
go version go1.24.4 windows/amd64

$ gofmt -l .
（无输出，exit 0）

$ go vet ./...
（无输出，exit 0）

$ go build ./...
（无输出，exit 0）

$ go test ./... -count=1
ok  github.com/lsong98sh/pairlink                3.264s
ok  .../internal/admin 5.577s   ok  .../internal/agent 8.850s
ok  .../internal/client 7.349s  ok  .../internal/config 8.150s
ok  .../internal/forward 2.556s ok  .../internal/logx 1.653s
ok  .../internal/pairing 1.321s ok  .../internal/protocol 2.010s
ok  .../internal/reconnect 1.590s ok .../internal/registry 1.252s
ok  .../internal/relay 10.254s  ok  .../internal/service 2.016s
ok  .../internal/token 1.349s   ok  .../internal/tunnelqueue 1.346s
ok  .../internal/version 1.488s ok  github.com/lsong98sh/pairlink/test 45.916s
（exit 0）

$ go test -race ./... -count=1
17 个包全部 ok（relay 11.055s / test 27.416s / client 12.075s …），日志中无 "DATA RACE"、无 panic（exit 0）

$ go test ./test/... -count=2
ok  github.com/lsong98sh/pairlink/test  38.729s（exit 0，无 flake）
```

**最小复现 1（P0-1，内存放大）**：`E:\TEMP\review-patch\alloc.go`（`replace` 到本仓库，未改仓库文件）

```
5 字节数据报（type=0x0C，声明 payload_len=1 MiB，实际无负载）：
err = protocol: 读取 bind 负载: EOF
200 次 → 累计分配 200.1 MiB（每次 1024.4 KiB）
```

**最小复现 2（管理页与 ini 补丁）**：`E:\TEMP\review-patch\main.go`

```
== crlf+bom+no-trailing-nl：PatchValue 后 CRLF/BOM/无结尾换行原样保留，Load 读回 42s ✓
== dup-section+dup-key：[relay] 出现两次、pair_timeout 出现两次 → 两处都被替换为 42s，Load=42s ✓
== key-absent-insert：插入到段内最后一个非空行（注释行）之后，Load=42s ✓
== empty-file：生成 "[relay]\npair_timeout = 42s\n"，Load=42s ✓
== idempotent patch：同值重复 Patch 字节级一致（true）✓
== inject newline / CR：config: 段名/键名/值不能包含换行 ✓
== key with '='：config: 键名 "a=b" 不能包含 '=' ✓
== section bracket：config: 段名 "cli[ent" 不能包含方括号 ✓
```

**实跑管理页（P1-2 证据）**：用仓库根 `go build` 出的二进制 + 临时 `relay.ini`（`manage_listen=127.0.0.1:19000`）

```
GET /relay → HTTP 200
含 <meta http-equiv="refresh" content="1">   → True
同页 <form> 数量                              → 5
```

**文档链接抽查**：`docs/traceability.md` 共 564 条 `../path#Lnn` 链接 → 0 条文件缺失、0 条行号越界；231 条测试锚点中 38 条与 `func Test...` 实际起始行不符（P2-3）。

---

## 9. 是否建议发布 v0.3.0

**不建议直接发布当前 HEAD。** 理由按权重：

1. **P0-1** 是"未认证、低成本、远程"的可用性缺陷，且恰好违反文档已写明的安全承诺——公网 relay 上必须先修；
2. **P1-1/P1-2** 让 v0.3.0 的主打功能（管理页写回）在**按文档部署**和**按默认页面状态**下都不可用，用户第一印象会是"这个版本的管理页写坏了"；
3. 其余 P2/P3 属于工程质量与文档一致性问题，不阻断发布，但建议与 P0/P1 同批修（成本都很低）。

**建议**：按 §7 的顺序修 **1→2→3**（合计约 1 人日）后发布 **v0.3.1**；P2 批量（第 4、5 项）可作为同一版本的附加修复；P3 归入 v0.4.0 的文档清理。修完后本项目的质量评级可以到 **A-**：它有清晰的边界、可靠的清理语义、诚实的文档和真正在跑的 e2e——这三点是"陌生团队能接住"的核心。
