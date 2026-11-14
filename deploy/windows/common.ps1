# ============================================================================
# PairLink Windows 部署公共函数库（内部文件，**不是入口脚本**）。
# ----------------------------------------------------------------------------
# 由 deploy\windows\install-relay.ps1 / install-agent.ps1 / install-client.ps1
# 通过 `. (Join-Path $PSScriptRoot 'common.ps1')` 引用；只定义函数，不执行安装。
# 三个入口各自只操作自己那个模式的服务（pairlink-<mode>）。
# ============================================================================

if ($MyInvocation.InvocationName -ne '.') {
    Write-Warning 'common.ps1 是内部函数库，不是入口；请运行 install-relay.ps1 / install-agent.ps1 / install-client.ps1。'
}

function Resolve-PairLinkFile {
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
        return Resolve-PairLinkFile -Path $ExplicitPath -What 'pairlink.exe'
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

function Get-PairLinkServiceName {
    param([Parameter(Mandatory = $true)][string]$Mode)
    return 'pairlink-' + $Mode
}

function Get-PairLinkLogPath {
    param(
        [Parameter(Mandatory = $true)][string]$BinaryPath,
        [Parameter(Mandatory = $true)][string]$ServiceName
    )
    return (Join-Path (Split-Path -Parent $BinaryPath) ($ServiceName + '.log'))
}

function Show-PairLinkServiceState {
    param([Parameter(Mandatory = $true)][string]$ServiceName)
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($null -eq $service) {
        Write-Host ("  当前状态 : 未安装（{0}）" -f $ServiceName) -ForegroundColor Yellow
        return $null
    }
    Write-Host ("  当前状态 : {0}（启动类型 {1}）" -f $service.Status, $service.StartType)
    return $service
}

# 统一调用 pairlink.exe 的 -<mode> [-config <abs>] -service <action>。
# 返回进程退出码；调用方负责判断并把错误翻译成可读提示。
function Invoke-PairLinkServiceCommand {
    param(
        [Parameter(Mandatory = $true)][string]$BinaryPath,
        [Parameter(Mandatory = $true)][string]$Mode,
        [Parameter(Mandatory = $true)][string]$Action,
        [string]$ConfigPath
    )
    $argumentList = @('-' + $Mode)
    if (-not [string]::IsNullOrWhiteSpace($ConfigPath)) {
        $argumentList += @('-config', $ConfigPath)
    }
    $argumentList += @('-service', $Action)

    Write-Host ("  执行: `"{0}`" {1}" -f $BinaryPath, ($argumentList -join ' ')) -ForegroundColor DarkGray
    # 用 Out-Host 让 pairlink 自己的输出只显示、不混进函数返回值（否则 $code 会变成数组）。
    & $BinaryPath @argumentList | Out-Host
    $exitCode = $LASTEXITCODE
    if ($null -eq $exitCode) {
        return 0
    }
    return $exitCode
}

function Remove-PairLinkLog {
    param(
        [string]$LogPath,
        [bool]$KeepLogs
    )
    if ([string]::IsNullOrWhiteSpace($LogPath)) {
        return
    }
    if (-not (Test-Path -LiteralPath $LogPath -PathType Leaf)) {
        Write-Host ("  日志文件 : 不存在（{0}）" -f $LogPath)
        return
    }
    if ($KeepLogs) {
        Write-Host ("  日志文件 : 已保留（{0}）" -f $LogPath) -ForegroundColor Yellow
        return
    }
    Remove-Item -LiteralPath $LogPath -Force
    Write-Host ("  日志文件 : 已删除（{0}）" -f $LogPath)
}
