# PairLink Windows 服务部署

本目录按角色拆成三个入口脚本，**每个脚本只操作自己那个服务**（服务名固定
`pairlink-<mode>`），底层调用 `main.go` 的 `-service install|start|stop|uninstall`
子命令，不额外引入依赖。`common.ps1` 是内部函数库（参数/文件解析、服务状态、日志路径、
`-service` 调用），不是部署入口。

| 文件 | 作用 |
| --- | --- |
| `install-relay.ps1` | 只安装/卸载 `pairlink-relay`（配置文件 `relay.ini`） |
| `install-agent.ps1` | 只安装/卸载 `pairlink-agent`（配置文件 `agent.ini`） |
| `install-client.ps1` | 只安装/卸载 `pairlink-client`（配置文件 `client.ini`） |
| `common.ps1` | 内部函数库；只放函数，不执行安装 |

> **一台机器通常只部署一个角色**：三个脚本各自只安装并启动自己的服务，不会顺带拉起
> 另外两个。单机演示要跑三个角色时，分别执行三个脚本即可。

## 1. 前置条件（必读）

1. **必须是“以管理员身份运行”的 PowerShell**。脚本首行是
   `#Requires -RunAsAdministrator`，非管理员会话会在执行前直接报错退出。
   - 开始菜单搜索 "PowerShell" → 右键 **以管理员身份运行**；
   - 或 `Start-Process pwsh -Verb RunAs`；
   - 确认：`([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)` 应为 `True`。
2. 已构建 `pairlink.exe`（`go build -o pairlink.exe .`），并把 `deploy\windows` 与
   `examples\` 一起取到本机。
3. 配置里的路径建议用**绝对路径**；脚本会先把 `-Config` 解析成绝对路径再交给 SCM
   （SCM 启动服务时工作目录不是 exe 目录）。
4. 执行策略若拦截脚本，用：
   ```powershell
   powershell -NoProfile -ExecutionPolicy Bypass -File .\install-relay.ps1 -Config C:\PairLink\relay.ini
   ```

## 2. 安装

```powershell
# 在管理员 PowerShell 中，进入 deploy\windows（或写完整路径）
cd E:\BizWorks\pairlink\deploy\windows

# relay（公网中继）——只装 pairlink-relay
.\install-relay.ps1 -Config C:\PairLink\relay.ini

# agent（服务提供者）——只装 pairlink-agent；二进制不在默认位置时用 -Binary 指定
.\install-agent.ps1 -Config C:\PairLink\agent.ini -Binary C:\PairLink\pairlink.exe -NoStart

# client——只装 pairlink-client
.\install-client.ps1 -Config C:\PairLink\client.ini
```

- **服务名固定为 `pairlink-<mode>`**：`pairlink-relay` / `pairlink-agent` / `pairlink-client`；
- 启动类型为“自动”，开机自启；默认安装后立即 `start`，`-NoStart` 只注册不启动；
- **日志固定写到 `pairlink.exe` 同目录的 `pairlink-<mode>.log`**（服务没有控制台，
  stdout/stderr 也重定向到该文件）。把 exe 放在 `C:\PairLink\` 时日志就是
  `C:\PairLink\pairlink-relay.log`；
- 服务以 LocalSystem 运行，对 exe 所在目录有写权限；如果手动改成受限账号，
  必须保证该账号能写日志文件、能读配置文件。

### 幂等与重装

服务已存在时，脚本默认打印状态并退出（退出码 0），不修改任何东西。要重装：

```powershell
.\install-relay.ps1 -Config C:\PairLink\relay.ini -Force
```

`-Force` 会先 `-service stop`、再 `-service uninstall`，然后重新 install/start。

## 3. 查看状态与日志

```powershell
sc.exe query pairlink-relay
Get-Service pairlink-relay
Get-Service pairlink-relay | Format-List Name,Status,StartType

# 实时看日志（Ctrl+C 退出）
Get-Content -Wait C:\PairLink\pairlink-relay.log
Get-Content C:\PairLink\pairlink-relay.log -Tail 100
```

`sc.exe query` 的 `STATE` 为 `4 RUNNING` 表示运行中；`STOPPED` 为已停止。
服务启动异常时先看日志文件，再用 `sc.exe qc pairlink-relay` 确认 `BINARY_PATH_NAME`
里的 exe 与 `-config` 绝对路径是否正确。

常用控制：

```powershell
Start-Service pairlink-relay
Stop-Service pairlink-relay
Restart-Service pairlink-relay
```

> `-service` 子命令**仅 Windows 可用**；Linux 请用 `deploy/linux/` + `deploy/systemd/`，
> 不要给 systemd 的 `ExecStart` 加 `-service`。

## 4. 防火墙

在 relay 上按需放行（管理员 PowerShell）：

```powershell
New-NetFirewallRule -DisplayName 'PairLink relay TCP' -Direction Inbound -Protocol TCP -LocalPort 7000,7001 -Action Allow
New-NetFirewallRule -DisplayName 'PairLink relay UDP' -Direction Inbound -Protocol UDP -LocalPort 7002 -Action Allow
# 管理页不要对公网放行；需要远程看就用 SSH 隧道/内网
```

agent/client 一般只需出站，不需要入站规则。

## 5. 卸载

```powershell
# 默认：停止并卸载本模式服务，删除 exe 同目录的 pairlink-<mode>.log；保留 ini/证书
.\install-relay.ps1 -Uninstall

# 保留日志用于事后排查
.\install-relay.ps1 -Uninstall -KeepLogs

# 其它角色分别用 install-agent.ps1 / install-client.ps1 的 -Uninstall
```

服务不存在时脚本视为已卸载成功（退出码 0），可安全重复执行。
**ini 配置与证书不会被删除**；需要清理请手动删除配置目录。

## 6. 排错

| 现象 | 原因 / 处理 |
| --- | --- |
| 脚本直接报 `#requires` / 拒绝运行 | 当前不是管理员 PowerShell；重新“以管理员身份运行” |
| `CreateService ... Access is denied` / 退出码 5 | 同上，SCM 需要管理员权限 |
| `服务 pairlink-<mode> 已存在` | 用 `-Force` 重装，或先 `install-<mode>.ps1 -Uninstall` |
| `启动服务失败` / 服务 1053 | 看 `pairlink-<mode>.log`；多为配置路径、token、证书或端口占用问题 |
| 日志文件不存在 | 服务根本没有运行到写日志阶段；先看 `sc.exe qc` 的 `BINARY_PATH_NAME` 与事件查看器 |
| 配置改了不生效 | Windows 服务不会热加载 ini；`Restart-Service pairlink-<mode>` |
| 端口被占 | `Get-NetTCPConnection -LocalPort 7000,7001 -ErrorAction SilentlyContinue` |
| 卸载后服务仍显示 | SCM 停止需要时间；稍后 `sc.exe query`，必要时重启后删除 |
| 中文乱码 | 四个 `.ps1` 已保存为 UTF-8 with BOM，兼容 Windows PowerShell 5.1；仍乱码时优先用 PowerShell 7（`pwsh`） |

## 7. 脚本自身检查（开发机）

```powershell
Get-ChildItem .\*.ps1 | ForEach-Object {
    $errors = $null
    [System.Management.Automation.Language.Parser]::ParseFile(
        $_.FullName, [ref]$null, [ref]$errors) | Out-Null
    "{0}: errors={1}" -f $_.Name, $errors.Count   # 必须全为 0
}
```

注意：解析通过只证明语法正确；`install/start/stop/uninstall` 的真实 SCM 生命周期
需要管理员环境验证（本仓库开发机非管理员，未执行）。

## 8. 从旧脚本迁移

阶段 6F 起删除了 `install-service.ps1` / `uninstall-service.ps1`。迁移方式：

| 旧命令 | 新命令 |
| --- | --- |
| `.\install-service.ps1 -Mode relay -Config X` | `.\install-relay.ps1 -Config X` |
| `.\install-service.ps1 -Mode agent -Config X -Force` | `.\install-agent.ps1 -Config X -Force` |
| `.\uninstall-service.ps1 -Mode relay -KeepLogs` | `.\install-relay.ps1 -Uninstall -KeepLogs` |

参数 `-Config` / `-Binary` / `-Force` / `-KeepLogs` / `-NoStart` 与旧脚本一致；
服务名、日志路径、SCM 行为不变，已安装的服务无需迁移。
