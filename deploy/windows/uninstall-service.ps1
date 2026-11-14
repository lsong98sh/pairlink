#Requires -RunAsAdministrator
<#
.SYNOPSIS
    卸载 PairLink 的 Windows 服务（SCM）。

.DESCRIPTION
    先调用 pairlink.exe 的 `-<mode> -service stop`，再调用 `-service uninstall`。
    服务不存在时视为成功（幂等），退出码 0。
    默认删除 pairlink.exe 同目录的 pairlink-<mode>.log；-KeepLogs 保留日志。
    ini 配置、证书等文件不会被删除。

    必须使用“以管理员身份运行”的 PowerShell。

.PARAMETER Mode
    relay / agent / client。

.PARAMETER Binary
    pairlink.exe 路径。默认依次查找脚本同目录、deploy 目录、仓库根目录、当前目录。

.PARAMETER KeepLogs
    保留 pairlink-<mode>.log（默认删除）。

.EXAMPLE
    .\uninstall-service.ps1 -Mode relay

.EXAMPLE
    .\uninstall-service.ps1 -Mode agent -KeepLogs
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, HelpMessage = 'relay / agent / client')]
    [ValidateSet('relay', 'agent', 'client')]
    [string]$Mode,

    [string]$Binary,

    [switch]$KeepLogs
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-ExistingFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$What
    )
    try {
        $resolved = Resolve-Path -LiteralPath $Path -ErrorAction Stop
    }
    catch {
        throw "$What 不存在或不可访问: $Path"
    }
    $full = $resolved.ProviderPath
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
        throw "$What 不是普通文件: $full"
    }
    return $full
}

function Find-PairLinkBinary {
    param([string]$ExplicitPath)

    if (-not [string]::IsNullOrWhiteSpace($ExplicitPath)) {
        return Resolve-ExistingFile -Path $ExplicitPath -What 'pairlink.exe'
    }

    $scriptRoot = $PSScriptRoot
    if ([string]::IsNullOrWhiteSpace($scriptRoot)) {
        $scriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
    }
    $deployRoot = Split-Path -Parent $scriptRoot
    $repoRoot = Split-Path -Parent $deployRoot

    $candidates = @(
        (Join-Path $scriptRoot 'pairlink.exe'),
        (Join-Path $deployRoot 'pairlink.exe'),
        (Join-Path $repoRoot 'pairlink.exe'),
        (Join-Path (Get-Location).Path 'pairlink.exe')
    )
    foreach ($candidate in $candidates) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) {
            return (Resolve-Path -LiteralPath $candidate).ProviderPath
        }
    }
    return $null
}

$modeArg = '-' + $Mode
$serviceName = 'pairlink-' + $Mode

Write-Host "PairLink Windows 服务卸载"
Write-Host ("  模式   : {0}" -f $Mode)
Write-Host ("  服务名 : {0}" -f $serviceName)
Write-Host ''

$existing = Get-Service -Name $serviceName -ErrorAction SilentlyContinue

# 服务不存在时也尽量定位二进制，以便清理日志；找不到不算错误。
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
    Write-Host ("服务 {0} 不存在，无需卸载。" -f $serviceName) -ForegroundColor Yellow
}
else {
    if ($null -eq $binaryPath) {
        throw '服务存在，但找不到 pairlink.exe；请用 -Binary 指定路径后再卸载。'
    }

    Write-Host '停止服务...'
    & $binaryPath $modeArg -service stop
    if ($LASTEXITCODE -ne 0) {
        throw ("停止服务失败（退出码 {0}）。未执行卸载；可先用 sc.exe query {1} 排查。" -f $LASTEXITCODE, $serviceName)
    }

    Write-Host '卸载服务（SCM）...'
    & $binaryPath $modeArg -service uninstall
    if ($LASTEXITCODE -ne 0) {
        throw ("卸载服务失败（退出码 {0}）。可先用 sc.exe query {1} 排查。" -f $LASTEXITCODE, $serviceName)
    }

    $stillThere = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    if ($null -ne $stillThere) {
        throw ("服务 {0} 仍存在（状态: {1}）。SCM 可能仍在停止中，请稍后重试。" -f $serviceName, $stillThere.Status)
    }
    Write-Host ("服务 {0} 已卸载。" -f $serviceName) -ForegroundColor Green
}

if ($null -ne $binaryPath) {
    $logPath = Join-Path (Split-Path -Parent $binaryPath) ($serviceName + '.log')
    if ($KeepLogs) {
        if (Test-Path -LiteralPath $logPath -PathType Leaf) {
            Write-Host ("保留日志: {0}" -f $logPath)
        }
    }
    elseif (Test-Path -LiteralPath $logPath -PathType Leaf) {
        Remove-Item -LiteralPath $logPath -Force
        Write-Host ("已删除日志: {0}" -f $logPath)
    }
    else {
        Write-Host ("日志不存在，跳过: {0}" -f $logPath)
    }
}

Write-Host ''
Write-Host '卸载完成；ini 配置与证书文件未删除。' -ForegroundColor Green
Write-Host ("如需复查服务是否残留: sc.exe query {0}" -f $serviceName)
