# PairLink systemd 部署（Linux）

本目录提供三个 unit，一种模式一个，固定服务名与 Windows 保持一致：

| unit | 模式 | 配置文件 | 默认端口 |
| --- | --- | --- | --- |
| `pairlink-relay.service` | relay（公网中继） | `/etc/pairlink/relay.ini` | 7000/tcp、7001/tcp、7002/udp（可选）、9000/tcp（管理页，仅本机） |
| `pairlink-agent.service` | agent（服务提供者） | `/etc/pairlink/agent.ini` | 出站到 relay，无入站 |
| `pairlink-client.service` | client（访问侧） | `/etc/pairlink/client.ini` | 本机 `service.<ID>` 监听端口 |

> **Linux 不要加 `-service`**：`-service install|start|stop|uninstall` 是 Windows SCM 专用；
> Linux 上由 systemd 直接托管前台进程，进程收到 `SIGTERM` 会优雅退出（`TimeoutStopSec=15s`）。

## 1. 安装

### 方式 A：用仓库脚本（推荐）

按角色执行对应的一个脚本（**每个脚本只安装/启动自己那个服务**）：

```bash
sudo ./deploy/linux/install-relay.sh --binary ./pairlink
sudo ./deploy/linux/install-agent.sh --binary ./pairlink
sudo ./deploy/linux/install-client.sh --binary ./pairlink
```

脚本会：安装共享二进制 → 创建 `/etc/pairlink`（0750、`root:pairlink`）→ 只在缺失时安装
该角色的示例 ini（不覆盖已有配置）→ 安装该角色的 unit → `daemon-reload` → `enable` → `start`
（`--no-start` 只 enable；`--no-systemd` 不装 unit）。一台机器通常只部署一个角色；
需要三个角色都跑时分别执行三个脚本，不需要先装三个再 disable。

### 方式 B：手工安装

（`pairlink` 用户/组不存在时，先执行本节末尾的创建命令。）

```bash
sudo install -m 0755 pairlink /usr/local/bin/pairlink
sudo install -d -m 0750 -o root -g pairlink /etc/pairlink
sudo install -m 0640 -o root -g pairlink examples/relay.ini /etc/pairlink/relay.ini
sudo install -m 0644 deploy/systemd/pairlink-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now pairlink-relay
```

`pairlink` 用户不存在时先创建（`install-<mode>.sh` 会自动做）：

```bash
sudo groupadd --system pairlink
sudo useradd --system --gid pairlink --no-create-home --shell /usr/sbin/nologin pairlink
```

## 2. 以非 root 运行（推荐）

三个 unit 默认注释掉了 `User=pairlink`。启用步骤：

1. 创建 `pairlink` 系统用户（见上）；
2. 配置与证书只给 root + pairlink 读：
   ```bash
   sudo chown -R root:pairlink /etc/pairlink
   sudo chmod 0750 /etc/pairlink
   sudo chmod 0640 /etc/pairlink/*.ini
   sudo chmod 0640 /etc/pairlink/tls/* 2>/dev/null || true
   ```
   **需要管理页写回 ini 时**（修改参数 / 服务增删改），目录必须对服务用户可写，
   因为写回是「同目录临时文件 + rename」（只读目录会报 `permission denied`/`EROFS`）：
   ```bash
   # 方案 A：目录属主给 pairlink（推荐）
   sudo chown -R pairlink:pairlink /etc/pairlink
   # 方案 B：保留 root:pairlink，目录组可写 + setgid，文件组可写
   sudo chmod 2775 /etc/pairlink
   sudo chmod 0660 /etc/pairlink/*.ini
   ```
   另外 unit 里的 `ReadWritePaths=/etc/pairlink` 必须保留（`ProtectSystem=strict` 下
   只有该路径可写）；不用页面写回时可把它注释掉，恢复全盘只读（P1-1）。
3. `sudo systemctl edit pairlink-relay` 写入：
   ```ini
   [Service]
   User=pairlink
   Group=pairlink
   ```
   或直接编辑 unit 取消注释后 `daemon-reload`。

unit 已保留 `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` 与 `AmbientCapabilities=CAP_NET_BIND_SERVICE`，
因此非 root 绑 1024 以下端口（如把 relay 改到 443）也能工作。

## 3. 权限与加固说明

- `ProtectSystem=strict` 把整个文件系统挂成只读；unit 已用 `ReadWritePaths=/etc/pairlink`
  只为配置目录开一个写口（管理页写回需要临时文件 + rename，P1-1）。证书不需要额外放行；
  如果自定义证书放到别处，请确保该路径在只读视图内可读。
- `ProtectHome=true` 会隐藏 `/home`；不要把配置/证书放到用户家目录。
- `NoNewPrivileges=true`、`PrivateTmp=true`、`PrivateDevices=true`、空能力集（仅留低端口绑定）等
  是"适度加固"：正常代理转发不需要写盘、不需要 root 能力。排障时可临时
  `sudo systemctl edit pairlink-relay` 覆盖单个选项。
- 日志只进 journal，不落盘；需要长期保留请配置 journald 的 `Storage=`/`SystemMaxUse=` 或转发。
- **不要把 token 写进 unit 的 `Environment=`**：环境变量会出现在 `systemctl show` 与
  `/proc/<pid>/environ`。token 只放 `/etc/pairlink/*.ini`（0640）。

## 4. 启停、状态与日志

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now pairlink-relay
sudo systemctl status pairlink-relay --no-pager
sudo systemctl restart pairlink-relay
sudo systemctl stop pairlink-relay
sudo systemctl disable --now pairlink-relay

# 日志（结构化字段 event=... 便于检索）
sudo journalctl -u pairlink-relay -n 100 --no-pager
sudo journalctl -u pairlink-relay -f
sudo journalctl -u pairlink-relay --since "10 min ago" -o cat
sudo journalctl -u pairlink-relay -o json-pretty | head
```

启动成功时 journal 里应看到 `event=start`（`main.go` 的 "PairLink 启动"），
管理页 `manage_listen` 所在端口可用 `curl -fsS http://127.0.0.1:9000/ >/dev/null && echo ok` 验证。

## 5. 排错

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| `status=203/EXEC` | `/usr/local/bin/pairlink` 不存在或不可执行 | 重新 `install -m 0755`；`ls -l /usr/local/bin/pairlink` |
| `status=217/USER` | unit 启用了 `User=pairlink` 但用户不存在 | 创建用户或先注释掉 `User=` |
| `status=226/NAMESPACE` | 加固项与自定义路径冲突（例如证书在 `/home`） | `systemctl edit` 放宽 `ProtectHome=`/`ProtectSystem=` |
| `permission denied` 读配置/证书 | 目录 0750 `root:pairlink`，但进程是 root 以外的用户却无组权限 | `chgrp pairlink` + `chmod 0640` |
| 反复重启后 `start-limit-hit` | 配置错误/端口占用导致进程秒退，触发 60s/5 次保护 | 先看 `journalctl -u ... -n 50`，修好后 `systemctl reset-failed pairlink-relay` 再 start |
| 端口被占 | 已有实例或其它程序 | `ss -ltnup \| grep -E ':(7000\|7001\|7002)'` |
| 明文告警 `未配置 tls_cert / tls_key` | 示例配置默认不启用 TLS | 生产按 `examples/relay.ini` 注释配置证书，并给 agent/client 配 `tls_ca` |

## 6. 卸载

```bash
# 按角色卸载：只删本模式 unit；默认保留 /etc/pairlink
sudo ./deploy/linux/install-relay.sh --uninstall
sudo ./deploy/linux/install-agent.sh --uninstall
sudo ./deploy/linux/install-client.sh --uninstall

# 额外删除本模式 ini 与明确属于本模式的证书（不删其它模式的）
sudo ./deploy/linux/install-relay.sh --uninstall --purge
```

共享二进制 `/usr/local/bin/pairlink` 只有在**没有任何其它 `pairlink-*.service` 残留**时才删除，
所以先卸载 relay 不会把还在运行的 agent/client 的二进制删掉。`pairlink` 用户/组默认保留，
确认不再使用后再手动 `userdel` / `groupdel`。

## 7. 验证 unit 语法（任何装有 systemd 的机器）

```bash
systemd-analyze verify deploy/systemd/pairlink-relay.service \
                       deploy/systemd/pairlink-agent.service \
                       deploy/systemd/pairlink-client.service
```
