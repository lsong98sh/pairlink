#Requires -RunAsAdministrator
<#
.SYNOPSIS
    把 PairLink 安装为 Windows 服务（SCM）。

.DESCRIPTION
    调用 pairlink.exe 的 `-<mode> -config <绝对路径> -service install`，
    然后 `-service start`。服务名固定为 pairlink-<mode>，启动类型为自动；
    服务日志写到 pairlink.exe 同目录的 pairlink-<mode>.log。

    必须使用“以管理员身份运行”的 PowerShell。脚本是幂等的：
    服务已存在时默认只提示、不做修改；加 -Force 会先 stop + uninstall 再重装。

.PARAMETER Mode
    relay / agent / client。

.PARAMETER Config
    ini 配置文件的路径（相对路径会被解析为绝对路径；文件必须存在）。

.PARAMETER Binary
    pairlink.exe 路径。默认依次查找脚本同目录、deploy 目录、仓库根目录、当前目录。

.PARAMETER Force
    服务已存在时先停止并卸载再重新安装。

.EXAMPLE
    .\install-service.ps1 -Mode relay -Config C:\PairLink\relay.ini

.EXAMPLE
    .\install-service.ps1 -Mode agent -Config .\agent.ini -Binary C:\PairLink\pairlink.exe -Force
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, HelpMessage = 'relay / agent / client')]
    [ValidateSet('relay', 'agent', 'client')]
    [string]$Mode,

    [Parameter(Mandatory = $true, HelpMessage = 'ini 配置文件路径')]
    [string]$Config,

    [string]$Binary,

    [switch]$Force
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
    throw '未找到 pairlink.exe：请用 -Binary 指定路径，或把 pairlink.exe 放到脚本同目录/仓库根目录。'
}

$modeArg = '-' + $Mode
$serviceName = 'pairlink-' + $Mode

$binaryPath = Find-PairLinkBinary -ExplicitPath $Binary
$configPath = Resolve-ExistingFile -Path $Config -What '配置文件'

Write-Host "PairLink Windows 服务安装"
Write-Host ("  模式     : {0}" -f $Mode)
Write-Host ("  服务名   : {0}" -f $serviceName)
Write-Host ("  二进制   : {0}" -f $binaryPath)
Write-Host ("  配置文件 : {0}" -f $configPath)
Write-Host ''

$existing = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if ($null -ne $existing) {
    if (-not $Force) {
        Write-Host ("服务 {0} 已存在（状态: {1}），未做任何修改。" -f $serviceName, $existing.Status) -ForegroundColor Yellow
        Write-Host '如需重新安装，请加 -Force（会先停止并卸载旧服务）。'
        exit 0
    }
    Write-Host ("服务 {0} 已存在，-Force：先停止并卸载旧服务..." -f $serviceName) -ForegroundColor Yellow
    & $binaryPath $modeArg -config $configPath -service stop
    if ($LASTEXITCODE -ne 0) {
        Write-Warning ("停止旧服务返回退出码 {0}，继续尝试卸载。" -f $LASTEXITCODE)
    }
    & $binaryPath $modeArg -config $configPath -service uninstall
    if ($LASTEXITCODE -ne 0) {
        throw ("卸载旧服务失败（退出码 {0}）。请先手动执行: sc.exe query {1}" -f $LASTEXITCODE, $serviceName)
    }
}

Write-Host '安装服务（SCM，需要管理员权限）...'
& $binaryPath $modeArg -config $configPath -service install
if ($LASTEXITCODE -ne 0) {
    throw ("安装服务失败（退出码 {0}）。请确认已用管理员 PowerShell，且 {1} 可执行。" -f $LASTEXITCODE, $binaryPath)
}

Write-Host '启动服务...'
& $binaryPath $modeArg -config $configPath -service start
if ($LASTEXITCODE -ne 0) {
    throw ("启动服务失败（退出码 {0}）。查看服务日志：{1}" -f $LASTEXITCODE, (Join-Path (Split-Path -Parent $binaryPath) ($serviceName + '.log')))
}

$service = Get-Service -Name $serviceName -ErrorAction Stop
$logPath = Join-Path (Split-Path -Parent $binaryPath) ($serviceName + '.log')

Write-Host ''
Write-Host '安装完成。' -ForegroundColor Green
Write-Host ("  服务名     : {0}" -f $serviceName)
Write-Host ("  当前状态   : {0}" -f $service.Status)
Write-Host ("  二进制     : {0}" -f $binaryPath)
Write-Host ("  配置文件   : {0}" -f $configPath)
Write-Host ("  日志文件   : {0}" -f $logPath)
Write-Host ''
Write-Host '常用命令（管理员 PowerShell）：'
Write-Host ("  sc.exe query {0}" -f $serviceName)
Write-Host ("  Get-Service {0}" -f $serviceName)
Write-Host ("  Get-Content -Wait '{0}'" -f $logPath)
Write-Host ("  .\uninstall-service.ps1 -Mode {0}" -f $Mode)
