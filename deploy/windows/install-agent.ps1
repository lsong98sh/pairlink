#Requires -RunAsAdministrator
<#
.SYNOPSIS
    只安装/卸载 PairLink agent 的 Windows 服务（服务名固定 pairlink-agent）。

.DESCRIPTION
    本脚本只操作 agent 一个角色：调用 pairlink.exe 的
    `-agent -config <绝对路径> -service install|start|stop|uninstall`。
    **不会安装或启动 relay/client**——一台机器通常只部署一个角色。

    安装：校验二进制/配置存在 → 配置转绝对路径 → -service install → 默认 -service start。
    卸载：-service stop → -service uninstall；服务不存在视为成功（幂等）。
    日志固定写在 pairlink.exe 同目录的 pairlink-agent.log。
    ini 配置与证书不会被删除。

    必须使用“以管理员身份运行”的 PowerShell；非管理员会被 #Requires 直接拦截。

.PARAMETER Config
    ini 配置文件路径（相对路径会解析为绝对路径；安装时必须存在）。

.PARAMETER Binary
    pairlink.exe 路径。默认依次查找脚本同目录、deploy 目录、仓库根目录、当前目录。

.PARAMETER Uninstall
    停止并卸载 pairlink-agent 服务。

.PARAMETER Force
    服务已存在时先 stop + uninstall，再重新安装（仅安装时生效）。

.PARAMETER KeepLogs
    卸载时保留 pairlink-agent.log（仅卸载时生效；默认删除日志）。

.PARAMETER NoStart
    安装后只注册服务、不自动启动。

.EXAMPLE
    .\install-agent.ps1 -Config C:\PairLink\agent.ini

.EXAMPLE
    .\install-agent.ps1 -Config C:\PairLink\agent.ini -NoStart

.EXAMPLE
    .\install-agent.ps1 -Uninstall -KeepLogs
#>
[CmdletBinding()]
param(
    [string]$Config,

    [string]$Binary,

    [switch]$Uninstall,

    [switch]$Force,

    [switch]$KeepLogs,

    [switch]$NoStart
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'common.ps1')

$Mode = 'agent'
$ServiceName = Get-PairLinkServiceName -Mode $Mode

if ($Uninstall) {
    Write-Host 'PairLink Windows 服务卸载（agent）'
    Write-Host ("  服务名 : {0}" -f $ServiceName)
    Write-Host ''
    if (-not [string]::IsNullOrWhiteSpace($Config)) {
        Write-Warning '卸载不需要 -Config，已忽略。'
    }
    if ($Force -or $NoStart) {
        Write-Warning '-Force / -NoStart 仅在安装时生效，卸载时忽略。'
    }

    $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    $binaryPath = $null
    try {
        $binaryPath = Find-PairLinkBinary -ExplicitPath $Binary
    }
    catch {
        if ($null -ne $existing) {
            throw '服务存在，但找不到 pairlink.exe；请用 -Binary 指定路径后再卸载。'
        }
        Write-Warning $_.Exception.Message
    }

    if ($null -eq $existing) {
        Write-Host ("服务 {0} 不存在，视为已卸载（幂等）。" -f $ServiceName) -ForegroundColor Yellow
    }
    else {
        if ($null -eq $binaryPath) {
            throw '服务存在，但找不到 pairlink.exe；请用 -Binary 指定路径后再卸载。'
        }

        Write-Host '停止服务...'
        $code = Invoke-PairLinkServiceCommand -BinaryPath $binaryPath -Mode $Mode -Action stop
        if ($code -ne 0) {
            throw ("停止服务失败（退出码 {0}）。未执行卸载；可先用 sc.exe query {1} 排查。" -f $code, $ServiceName)
        }

        Write-Host '卸载服务（SCM）...'
        $code = Invoke-PairLinkServiceCommand -BinaryPath $binaryPath -Mode $Mode -Action uninstall
        if ($code -ne 0) {
            throw ("卸载服务失败（退出码 {0}）。可先用 sc.exe query {1} 排查。" -f $code, $ServiceName)
        }

        $stillThere = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($null -ne $stillThere) {
            throw ("服务 {0} 仍存在（状态: {1}）。SCM 可能仍在停止中，请稍后重试。" -f $ServiceName, $stillThere.Status)
        }
        Write-Host ("服务 {0} 已卸载。" -f $ServiceName) -ForegroundColor Green
    }

    if ($null -ne $binaryPath) {
        $logPath = Get-PairLinkLogPath -BinaryPath $binaryPath -ServiceName $ServiceName
        Remove-PairLinkLog -LogPath $logPath -KeepLogs ([bool]$KeepLogs)
    }

    Write-Host ''
    Write-Host '卸载完成；ini 配置与证书文件未删除。' -ForegroundColor Green
    Write-Host ("复查服务是否残留: sc.exe query {0}" -f $ServiceName)
    exit 0
}

if ([string]::IsNullOrWhiteSpace($Config)) {
    throw '安装 agent 时必须用 -Config 指定 ini 配置文件路径。'
}
if ($KeepLogs) {
    Write-Warning '-KeepLogs 仅在 -Uninstall 时生效，安装时忽略。'
}

$binaryPath = Find-PairLinkBinary -ExplicitPath $Binary
$configPath = Resolve-PairLinkFile -Path $Config -What '配置文件'

Write-Host 'PairLink Windows 服务安装（agent）'
Write-Host ("  模式     : {0}" -f $Mode)
Write-Host ("  服务名   : {0}" -f $ServiceName)
Write-Host ("  二进制   : {0}" -f $binaryPath)
Write-Host ("  配置文件 : {0}" -f $configPath)
Write-Host ''

$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($null -ne $existing -and -not $Force) {
    Write-Host ("服务 {0} 已存在（状态: {1}），未做任何修改。" -f $ServiceName, $existing.Status) -ForegroundColor Yellow
    Write-Host '如需重新安装，请加 -Force（会先停止并卸载旧服务）；卸载请用 -Uninstall。'
    exit 0
}
if ($null -ne $existing -and $Force) {
    Write-Host ("服务 {0} 已存在，-Force：先停止并卸载旧服务..." -f $ServiceName) -ForegroundColor Yellow
    $code = Invoke-PairLinkServiceCommand -BinaryPath $binaryPath -Mode $Mode -Action stop
    if ($code -ne 0) {
        Write-Warning ("停止旧服务返回退出码 {0}，继续尝试卸载。" -f $code)
    }
    $code = Invoke-PairLinkServiceCommand -BinaryPath $binaryPath -Mode $Mode -Action uninstall
    if ($code -ne 0) {
        throw ("卸载旧服务失败（退出码 {0}）。请先手动执行: sc.exe query {1}" -f $code, $ServiceName)
    }
}

Write-Host '安装服务（SCM，需要管理员权限）...'
$code = Invoke-PairLinkServiceCommand -BinaryPath $binaryPath -Mode $Mode -Action install -ConfigPath $configPath
if ($code -ne 0) {
    throw ("安装服务失败（退出码 {0}）。请确认已用管理员 PowerShell，且 {1} 可执行。" -f $code, $binaryPath)
}

if ($NoStart) {
    Write-Host '已按 -NoStart 跳过启动（服务已注册，启动类型为自动）。' -ForegroundColor Yellow
}
else {
    Write-Host '启动服务...'
    $code = Invoke-PairLinkServiceCommand -BinaryPath $binaryPath -Mode $Mode -Action start -ConfigPath $configPath
    if ($code -ne 0) {
        throw ("启动服务失败（退出码 {0}）。查看服务日志：{1}" -f $code, (Get-PairLinkLogPath -BinaryPath $binaryPath -ServiceName $ServiceName))
    }
}

Write-Host ''
Write-Host '安装完成。' -ForegroundColor Green
Write-Host ("  服务名   : {0}" -f $ServiceName)
$service = Show-PairLinkServiceState -ServiceName $ServiceName
$logPath = Get-PairLinkLogPath -BinaryPath $binaryPath -ServiceName $ServiceName
Write-Host ("  二进制   : {0}" -f $binaryPath)
Write-Host ("  配置文件 : {0}" -f $configPath)
Write-Host ("  日志文件 : {0}" -f $logPath)
Write-Host ''
Write-Host '常用命令（管理员 PowerShell）：'
Write-Host ("  sc.exe query {0}" -f $ServiceName)
Write-Host ("  Get-Service {0}" -f $ServiceName)
Write-Host ("  Get-Content -Wait '{0}'" -f $logPath)
Write-Host ("  .\install-agent.ps1 -Uninstall [-KeepLogs]")
Write-Host ''
Write-Host '下一步：'
Write-Host ("  1. 编辑 {0}：relay_addr 指向 relay 隧道地址；token 与 relay 的 auth_token 一致；" -f $configPath)
Write-Host '     local / service.<ID> 按本机真实服务修改；relay 用私有 CA 时配 tls_ca。'
Write-Host '  2. agent 只主动出站到 relay，一般无需入站防火墙规则；'
Write-Host '     需放行出站 7000/tcp、7001/tcp（原生 UDP 再加 7002/udp，按实际 ini 调整）。'
if ($null -eq $service) {
    Write-Host ("  注意：服务 {0} 当前未在 SCM 中查到，请用 sc.exe query 复核。" -f $ServiceName) -ForegroundColor Yellow
}
