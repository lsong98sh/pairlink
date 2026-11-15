# PairLink 部署运维件

本目录（以及仓库根的 `Dockerfile` / `.dockerignore`）提供四类可直接使用的运维件：

| 路径 | 内容 |
| --- | --- |
| `deploy/systemd/` | Linux systemd unit（relay / agent / client）+ 部署说明 |
| `deploy/linux/` | 按角色拆分的 `install-{relay,agent,client}.sh` + `common.sh`（systemd、配置目录、幂等、dry-run） |
| `deploy/windows/` | 按角色拆分的 `install-{relay,agent,client}.ps1` + `common.ps1` + 部署说明 |
| `Dockerfile`、`.dockerignore`、`deploy/docker-compose.yml` | 多阶段容器镜像 + relay/agent/client/nginx 演示编排 |
| `examples/` | 5 份主机侧 ini 模板 + `examples/docker/` 3 份容器侧 ini 模板 |

配置语法、模式职责、安全模型以仓库根 `README.md` 与 `docs/` 为准；本文件只讲**怎么部署**。

---

## 1. 配置模板（`examples/`）

| 模板 | 用途 | 管理页 | 关键端口 |
| --- | --- | --- | --- |
| `relay.ini` | relay，启用原生 UDP | `127.0.0.1:17090` | 17000/tcp、17001/tcp、17002/udp |
| `relay-no-udp.ini` | relay，不监听 UDP（`udp_mode = tcp_tunnel`） | `127.0.0.1:17090` | 17000/tcp、17001/tcp |
| `agent.ini` | agent，注册主服务 + TCP echo + UDP 演示 | `127.0.0.1:17091` | 出站到 relay |
| `client.ini` | client，TCP 映射（default / echo） | `127.0.0.1:17092` | 本机 18080、18081 |
| `client-udp.ini` | client，UDP 映射（demo-udp） | `127.0.0.1:17093` | 本机 18082 |
| `examples/docker/relay.ini` | 容器内 relay（管理页绑 `0.0.0.0`） | `0.0.0.0:17090` | 17000/17001/17002 |
| `examples/docker/agent.ini` | 容器内 agent（`relay:17000`、`demo-http:80`） | `0.0.0.0:17091` | 出站 |
| `examples/docker/client.ini` | 容器内 client（`0.0.0.0:18080`） | `0.0.0.0:17092` | 18080 |

### 1.1 使用方式

```bash
# 主机侧：复制后改 token / TLS / 地址
sudo install -d -m 0750 -o root -g pairlink /etc/pairlink
sudo install -m 0640 -o root -g pairlink examples/relay.ini /etc/pairlink/relay.ini
sudo pairlink -relay -config /etc/pairlink/relay.ini
```

- **占位 token**：三份主机模板的 `token` / `auth_token` 都是
  `CHANGE-ME-RELAY-AUTH-TOKEN`，三端必须一致，且**投产前必须替换**：
  `openssl rand -hex 32`。占位符能通过校验，但绝不是可用密钥。
- **TLS**：示例默认注释掉 `tls_cert` / `tls_key` / `tls_ca`，方便本机先跑通；
  relay 启动会打一条明文 WARN。公网部署必须成对配置证书，并给 agent/client 配 `tls_ca`
  （或仅在测试时 `tls_insecure_skip_verify = true`）。
- **端口**：主机模板使用 17000 段（避开 7000 默认端口与常见服务），仅用于本机演练；
  生产按实际网络改监听地址并放行防火墙。管理页不要绑 `0.0.0.0`。
- **注释语法**：`internal/config/ini.go` 只支持**整行**注释（`;` 或 `#`，允许前导空白），
  **不支持行尾注释**。因此模板的字段说明都写在上一行；若写成
  `key = value ; 说明`，`; 说明` 会成为值的一部分。`test/examples_test.go` 会拦截这种写法。
- **相对路径**：ini 里的证书路径相对 ini 所在目录解析；服务模式下建议写绝对路径。

### 1.2 模板自检

```bash
go test ./test -run TestExamples -v -count=1
```

该测试会遍历 `examples/*.ini` 与 `examples/docker/*.ini`，做 `config.Load` + `Validate(mode)`，
并断言必填字段非空、token 含 `CHANGE-ME`、无未知键、无行尾注释、管理页监听符合
“主机 127.0.0.1 / 容器 0.0.0.0”的约定。

---

## 2. Linux systemd（`deploy/systemd/`）

三个 unit：`pairlink-relay.service`、`pairlink-agent.service`、`pairlink-client.service`。

```bash
sudo install -m 0644 deploy/systemd/pairlink-*.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now pairlink-relay      # 按角色只启动需要的
sudo systemctl status pairlink-relay --no-pager
sudo journalctl -u pairlink-relay -f
```

关键设计：

- `Type=simple`，`ExecStart=/usr/local/bin/pairlink -relay -config /etc/pairlink/relay.ini`；
  **Linux 不加 `-service`**（那是 Windows SCM 专用）。
- `Restart=on-failure` + `RestartSec=3s`，并用 `StartLimitIntervalSec=60` / `StartLimitBurst=5`
  限制疯狂重启；agent/client 控制连接断开时先由**内置自动重连**恢复（阶段 6C 起
  `reconnect = true` 默认开启，见仓库 README §5.4/§5.5），重连持续失败、被踢出或配置错误
  导致进程退出时再由 systemd 拉起。设 `reconnect = false` 可回退到"断开即退出、交给 systemd"。
- `StartLimitIntervalSec` 位于 `[Unit]`（systemd ≥ 236）。
- `LimitNOFILE=65535`、`UMask=0077`、`StandardOutput/Error=journal`。
- 适度加固：`NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`、`PrivateTmp`、
  `PrivateDevices`、`ProtectKernel*`、`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`、
  `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` 等。`/etc/pairlink` 在 `strict` 下仍可读，
  证书无需额外 `ReadOnlyPaths`。
- **P1-1 配置目录写回**：unit 在 `ProtectSystem=strict` 旁加了 `ReadWritePaths=/etc/pairlink`，
  因为管理页写回 ini 用「同目录临时文件 + rename」，全只读会失败。启用 `User=pairlink` 时
  还需要 `/etc/pairlink` 对该用户可写（`chmod 2775` 或 `chown -R pairlink:pairlink`）；
  不用页面写回时可注释掉 `ReadWritePaths` 恢复全盘只读。
- `User=pairlink` / `Group=pairlink` 默认**注释掉**（即 root 运行）；启用方法与权限要求见
  [`deploy/systemd/README.md`](systemd/README.md)。unit 保留了 `CAP_NET_BIND_SERVICE`，
  非 root 绑低端口也能用。
- **unit 里不放 token**：`Environment=` 会出现在 `systemctl show` 与 `/proc/<pid>/environ`，
  token 只写 `/etc/pairlink/*.ini`。
- unit 语法验证：`systemd-analyze verify deploy/systemd/*.service`。

---

## 3. Linux 按角色安装/卸载脚本（`deploy/linux/`）

一台机器通常只部署一个角色。三个入口脚本**各自只安装/启动自己那个服务**：

| 脚本 | 安装的本模式 ini | 安装的本模式 unit |
| --- | --- | --- |
| `install-relay.sh` | `/etc/pairlink/relay.ini` | `pairlink-relay.service` |
| `install-agent.sh` | `/etc/pairlink/agent.ini` | `pairlink-agent.service` |
| `install-client.sh` | `/etc/pairlink/client.ini` | `pairlink-client.service` |

`common.sh` 是内部函数库（文件头已注明），由三个入口 source，不是部署入口。

```bash
# 预览（可非 root）
./deploy/linux/install-relay.sh --dry-run

# relay 机器：共享二进制 + relay.ini + pairlink-relay.service（enable + start）
sudo ./deploy/linux/install-relay.sh --binary ./pairlink

# agent 机器
sudo ./deploy/linux/install-agent.sh --binary ./pairlink

# client 机器
sudo ./deploy/linux/install-client.sh --binary ./pairlink

# 只放二进制与配置，不装 unit
sudo ./deploy/linux/install-client.sh --binary ./pairlink --no-systemd

# 卸载：只删本模式 unit；默认保留 /etc/pairlink
sudo ./deploy/linux/install-relay.sh --uninstall
# 额外删除本模式 ini 与明确属于本模式的证书（不删其它模式的）
sudo ./deploy/linux/install-relay.sh --uninstall --purge
```

共同行为：

- `set -euo pipefail`；非 root 且非 `--dry-run` 直接拒绝；
- 共享二进制 `/usr/local/bin/pairlink`：内容相同跳过；不同则先写临时文件再 rename
  （避免覆盖正在运行的二进制时 `ETXTBSY`）；
- 创建 `pairlink` 系统用户/组；`/etc/pairlink` 为 `0750 root:pairlink`，
  本模式 ini 为 `0640 root:pairlink`；
- **已有配置绝不覆盖**，且只安装自己那一份示例 ini；
- 安装本模式 unit → `daemon-reload` → `enable` → 默认 `start`；
  `--no-start` 只 enable；`--no-systemd` 只放二进制与配置；
- 卸载：`disable --now` 本模式 unit → 删除本模式 unit → `daemon-reload`；
  **共享二进制只在没有任何其它 `pairlink-*.service` 残留时才删除**
  （先卸载 relay 不会把还在运行的 agent 的二进制删掉）；
- 默认保留 `/etc/pairlink` 与 `pairlink` 用户/组；`--purge` 只删本模式 ini
  与明确属于本模式的证书（`tls/<mode>.crt`、`<mode>.key` 等）；
- 服务/文件不存在时视为成功（退出码 0），可重复执行；未检测到 systemd 时只删文件并告警；
- `--dry-run` 只打印将执行的命令，不落盘。

三个脚本 + `common.sh` 通过 `shellcheck 0.11.0`（`shellcheck -x` 无告警）与 `bash -n`。

> 本仓库在 Windows 上开发时 `core.filemode=false`，提交脚本需显式保留可执行位：
> `git add --chmod=+x deploy/linux/install-relay.sh deploy/linux/install-agent.sh deploy/linux/install-client.sh`；
> 否则 Linux 上克隆后只能 `bash install-relay.sh`，不能 `./install-relay.sh`。

---

## 4. Windows 服务（`deploy/windows/`）

同样按角色拆成三个入口，**每个只操作自己的服务**（服务名固定 `pairlink-<mode>`）：

| 脚本 | 作用 |
| --- | --- |
| `install-relay.ps1` | 只安装/卸载 `pairlink-relay` |
| `install-agent.ps1` | 只安装/卸载 `pairlink-agent` |
| `install-client.ps1` | 只安装/卸载 `pairlink-client` |

`common.ps1` 是内部函数库（参数/文件解析、服务状态、日志路径、`-service` 调用），不是入口。

```powershell
# 必须以管理员身份运行 PowerShell
.\deploy\windows\install-relay.ps1 -Config C:\PairLink\relay.ini
.\deploy\windows\install-agent.ps1 -Config C:\PairLink\agent.ini -Binary C:\PairLink\pairlink.exe
.\deploy\windows\install-client.ps1 -Config C:\PairLink\client.ini -NoStart

# 已存在时重装
.\deploy\windows\install-relay.ps1 -Config C:\PairLink\relay.ini -Force

# 卸载（-KeepLogs 保留日志）
.\deploy\windows\install-relay.ps1 -Uninstall
.\deploy\windows\install-relay.ps1 -Uninstall -KeepLogs
```

- 启动类型自动；日志固定 `pairlink.exe` 同目录的 `pairlink-<mode>.log`（stdout/stderr 也重定向进去）；
- 先校验二进制/配置存在，把 `-Config` 解析为绝对路径（SCM 的工作目录不是 exe 目录），
  再调用 `pairlink.exe -<mode> -config <abs> -service install|start`；
- 服务已存在时默认提示并退出，`-Force` 才 stop + uninstall + 重装；
- 卸载先 `-service stop` 再 `-service uninstall`，`-KeepLogs` 保留日志；ini/证书不删；
- 详细状态查询、日志、防火墙与排错见 [`deploy/windows/README.md`](windows/README.md)。

---

## 5. 容器化（`Dockerfile` + `docker-compose.yml`）

### 5.1 镜像设计

- 多阶段：`golang:1.24-alpine` 构建 → `gcr.io/distroless/static-debian12:nonroot` 运行；
- 构建参数：`CGO_ENABLED=0`、`-trimpath -buildvcs=false -ldflags="-s -w"` → 静态、可剥离二进制；
- 运行用户 `nonroot:nonroot`（uid/gid 65532）；
- `ENTRYPOINT ["/pairlink"]`，无默认参数；`EXPOSE 7000 7001 7002/udp 9000`（文档性，
  不会 publish 端口；compose 用 17000 段并显式映射）；
- 选 distroless 的理由：纯静态 Go 不需要 libc/包管理器，攻击面最小，自带 CA 证书；
- **没有镜像内 `HEALTHCHECK`**：distroless 没有 shell/curl/wget，加健康检查需要额外编译探针；
  可选做法见 5.3。排障用 `docker logs` 与宿主机 `curl`。

### 5.2 构建与运行

```bash
# 仓库根目录
docker build -t pairlink:v0.3.1 .

# 冒烟：版本
docker run --rm pairlink:v0.3.1 -version

# 前台跑 relay（配置目录读写挂载；容器内管理页需绑 0.0.0.0）
sudo chown -R 65532:65532 examples/docker   # 管理页写回需要 uid 65532 可写
docker run --rm --read-only --cap-drop ALL \
  -p 127.0.0.1:17090:17090 \
  -v "$PWD/examples/docker:/etc/pairlink:rw" \
  pairlink:v0.3.1 -relay -config /etc/pairlink/relay.ini
```

> 挂载的 ini/证书必须让容器内 uid 65532 可读：主机上 `chmod 0644`（或
> `chmod 0640` + `--user` 调整）。`0600 root` 的文件容器读不到。
> **管理页写回需要目录可写**：写回是「同目录临时文件 + rename」，单文件 `:ro` 挂载既不可写
> 也无法 rename，因此挂载**目录**（`-v …:/etc/pairlink:rw`）并让 uid 65532 拥有/可写该目录；
> 根文件系统仍可 `--read-only`（P1-1）。

### 5.3 Compose 演示（relay + agent + nginx + client）

```bash
cd deploy
docker compose up -d --build
docker compose ps

# 断言：三个管理页 200
curl -fsS -o /dev/null -w 'relay  %{http_code}\n' http://127.0.0.1:17090/
curl -fsS -o /dev/null -w 'agent  %{http_code}\n' http://127.0.0.1:17091/
curl -fsS -o /dev/null -w 'client %{http_code}\n' http://127.0.0.1:17092/

# 端到端：client 18080 -> relay -> agent -> nginx:80
curl -fsS http://127.0.0.1:18080/ | grep -qi 'welcome to nginx' && echo 'pairlink e2e OK'

docker compose logs --tail=50 relay agent client
docker compose down
```

- 只 publish 四个端口，且全部绑 `127.0.0.1`：`17090/17091/17092`（管理页）与
  `18080`（client 本地入口）；relay 的 17000/17001/17002 默认不映射，需要外部 agent/client
  接入时再放开（compose 文件里有注释）。
- `examples/docker/*.ini` 里管理页绑 `0.0.0.0` 是**容器内**地址；宿主机侧靠
  `127.0.0.1:17090:17090` 限制访问来源。这是容器网络与主机网络的关键差异：
  **容器内的 `127.0.0.1` 是容器自己，不是宿主机**。
- 三个 PairLink 服务只读根文件系统、`cap_drop: ALL`、`no-new-privileges`；
  **配置目录按读写挂载**（`../examples/docker:/etc/pairlink:rw`），否则管理页写回会因
  rename 失败而报错；宿主机先 `sudo chown -R 65532:65532 examples/docker`（P1-1）。
  nginx 演示服务用 tmpfs 提供可写缓存目录。
- 可选健康检查（不放进镜像）：用宿主机 curl 管理页，或在编排层加一个
  `curlimages/curl` sidecar 定期请求 `http://relay:17090/`；不要依赖 distroless 内的 shell。

### 5.4 用户侧验证命令（v0.5.1 已在 WSL2 + Docker Engine 29.1.3 实跑）

> **实测环境（v0.5.1）**：WSL2 Ubuntu 26.04 + Docker Engine 29.1.3 / Compose v2.40.3。
> Docker Hub 直连被墙，镜像经 `docker.m.daocloud.io` / `gcr.m.daocloud.io` 拉取；
> `golang:1.24-alpine` 因上游限流未能拉通，改用**本地 Go 1.24.4 工具链组装的同 tag 替身镜像**
> 完成构建验证（Dockerfile 的 `go mod download` / `go build` / distroless 阶段均真实执行）。
> 结果：镜像 21.4MB、`user=nonroot:nonroot`、`-version` = `pairlink v0.5.1`、
> `read_only` + `cap_drop: ALL` + `no-new-privileges` 下可运行；compose 四容器全部 `Up`，
> 三个管理页 200，`http://127.0.0.1:18080/` 经 client→relay→agent 拿到 nginx 欢迎页。

```bash
docker build -t pairlink:v0.5.1 .
docker run --rm pairlink:v0.5.1 -version
docker image inspect pairlink:v0.5.1 --format '{{.Config.User}} {{.Config.Entrypoint}}'
# 期望：nonroot:nonroot [/pairlink]

docker compose -f deploy/docker-compose.yml up -d --build
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:17090/relay
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/
docker compose -f deploy/docker-compose.yml down
```

> **容器内管理页是只读的**（实测）：容器里 `manage_listen` 必须绑 `0.0.0.0`，而"非回环即只读"
> 是既有安全策略，页面会显示红色横幅，所有写接口返回 **403**（日志 `admin_action_denied`）。
> 因此 compose 的目录读写挂载目前**用不上**（保留只为将来可能的回环模式）；
> 改容器配置请直接改宿主机 `examples/docker/*.ini`，然后 `docker compose restart <服务>`。

---

## 6. 权限与安全清单

1. **`/etc/pairlink`**：`0750 root:pairlink`，ini `0640 root:pairlink`。默认 unit 以 root 运行，
   所以 root 能读；启用 `User=pairlink` 后组权限生效。证书同理 `0640 root:pairlink`。
2. **token**：`CHANGE-ME-...` 只是占位符；三端一致并替换为随机长串。
   不要写进 systemd `Environment=`、PowerShell 命令行参数或 compose `environment`。
3. **TLS**：公网 relay 必须 `tls_cert` + `tls_key`；agent/client 配 `tls_ca`。
   两侧不匹配会握手失败（预期行为，不会自动降级）。
4. **管理页无鉴权**：只绑 `127.0.0.1`；容器场景用宿主机端口映射限制来源。
5. **数据面明文**：relay 是哑管道；HTTP/RDP 等明文协议经公网需自行加 TLS/VPN。
6. **容器**：`read_only` + `cap_drop: ALL` + `no-new-privileges`；非 root uid 65532。
   若把监听端口改到 1024 以下，需要显式加回 `CAP_NET_BIND_SERVICE`。
7. **systemd**：unit 的 `ProtectSystem=strict` 让全盘只读；不要把配置/证书放到 `/home`
   （`ProtectHome=true` 会隐藏）。
8. **Windows 服务**：以 LocalSystem 运行；日志写在 exe 同目录，确保该目录可写。

---

## 7. 验证状态（诚实清单）

| 项目 | 状态 | 证据/说明 |
| --- | --- | --- |
| `go test ./test -run TestExamples` | ✅ 通过 | 全部主机/容器示例模板 Load/Validate 通过 |
| PowerShell 语法解析 | ✅ 0 错误 | `Parser::ParseFile` 对四个 `.ps1` 均为 `errors.Count = 0`（阶段 6F） |
| 非管理员运行安装脚本 | ✅ 被拦截 | `#Requires -RunAsAdministrator` 报 `PermissionDenied`，退出码 1 |
| `go build ./...` / `go vet ./...` | ✅ 通过 | 工作树（并行子代理改完 `internal/**` 后）复跑通过 |
| shellcheck | ✅ 无告警 | 0.11.0 `shellcheck -x deploy/linux/common.sh deploy/linux/install-*.sh` |
| `systemd-analyze verify` | ✅ 退出码 0 | 三个 unit（临时复制为 0644、二进制已安装） |
| WSL2 真机 systemd（阶段 6D） | ✅ 通过 | 三 unit 同时安装时 `active/enabled`；管理页 17090/17091/17092 均 200；stop 后端口释放 |
| 按模式安装（阶段 6F） | ✅ 通过 | 只装 relay 后 `pairlink-agent` 报 `could not be found`，`/etc/pairlink/agent.ini` 不存在；管理页 17090 返回 200 |
| 按模式卸载与共享二进制（6F） | ✅ 通过 | 无残留 unit 才删 `/usr/local/bin/pairlink`；卸载 relay 时 agent unit 仍在 → 二进制保留、agent 仍 enabled |
| 幂等 / 重复卸载（6F） | ✅ 通过 | 第二次 install 跳过二进制复制与配置覆盖；重复 `--uninstall` 退出 0；`--purge` 只删本模式 ini/证书 |
| `--dry-run` / `--no-systemd`（6F） | ✅ 通过 | 非 root dry-run 只打印且不出现其它模式 unit 操作；`--no-systemd` 不放 unit、只放二进制与 ini |
| Docker 镜像构建 / compose | ✅ **通过（v0.5.1）** | WSL2 + Docker Engine 29.1.3 / Compose 2.40.3：多阶段构建成功（21.4MB，nonroot），`read_only`+`cap_drop:ALL` 可运行，compose 四容器 Up、三管理页 200、18080 经隧道取到 nginx 首页；镜像经 daocloud 镜像源拉取，`golang` 基础镜像因上游限流用本地 Go 1.24.4 组装同 tag 替身 |
| Windows SCM 完整生命周期 | ✅ **通过（v0.5.1）** | 管理员提权会话实测 install/start/stop/uninstall、按日日志落盘、管理页 200、卸载后 `sc query` 1060（README §13.20） |
| Windows `install-*.ps1` 脚本自身 | ✅ **通过（v0.5.1）** | 三脚本各自独立进程真跑：三个服务同时 Running、三管理页 200、relay 页同时列出 agent/client；`-Force` / `-NoStart` / 幂等重装 / `-Uninstall` / `-Uninstall -KeepLogs` 全部 exit 0；实测修掉 `common.ps1` 的 `.Count` 异常（卸载成功却 exit 1） |

WSL 验证环境：Ubuntu（内核 6.18 WSL2）、systemd 259（`is-system-running` 报 `degraded`，
但 unit 可正常启停）、Go 1.24.4。阶段 6D 的三 unit 验证使用 `git archive HEAD`（`65f7064`）
编译的二进制；阶段 6F 的按模式验证使用工作树 `CGO_ENABLED=0 go build -o /tmp/pairlink .` 产物。
部署脚本与 Go 实现解耦，两种产物均验证过。

---

## 8. 排错速查

| 现象 | 排查 |
| --- | --- |
| systemd `203/EXEC` | `/usr/local/bin/pairlink` 不存在/不可执行 |
| systemd `217/USER` | 启用了 `User=pairlink` 但用户不存在 |
| systemd `226/NAMESPACE` | 加固项与自定义路径冲突（如证书在 `/home`） |
| 服务反复重启后 `start-limit-hit` | 看 `journalctl -u pairlink-<mode> -n 50`；修好后 `systemctl reset-failed` |
| 管理页 502/拒绝 | 配置里 `manage_listen` 是否绑对地址；容器内是否 `0.0.0.0`；宿主端口映射是否 `127.0.0.1` |
| 容器内权限拒绝读配置 | uid 65532 对挂载文件无读权限；`chmod 0644` 或调整 `user:` |
| `docker compose` 报 `name` 不支持 | 升级 Compose v2（`name:` 需要较新的 v2） |
| Windows 服务启动失败 | `sc.exe qc pairlink-<mode>`、exe 同目录 `pairlink-<mode>.log`、配置绝对路径 |

更多 systemd 细节见 [`deploy/systemd/README.md`](systemd/README.md)，
Windows 细节见 [`deploy/windows/README.md`](windows/README.md)。
