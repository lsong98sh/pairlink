#Requires -Version 5.1
<#
.SYNOPSIS
    构建 Windows（默认）与可选跨平台产物，并在 dist/ 下生成可直接分发的 zip。

.DESCRIPTION
    与 build.sh 行为一致，只是面向 Windows 主机：
      * 版本号**只读** internal/version/version.go 的 Version 常量（唯一来源），
        -Version 只用于断言"源码版本 == 期望版本"，不会改写任何文件；
      * 统一构建参数：CGO_ENABLED=0 + -trimpath + -ldflags "-s -w"（静态、可分发）；
      * 默认构建 windows/amd64 与 windows/arm64 两个产物；
      * 默认只为 amd64 打 zip（与 v0.5.1 的 dist 布局一致），-ZipAll 让 arm64 也打包；
      * zip 内布局：<name>/pairlink.exe、LICENSE、README.md、deploy/**、examples/**。

    构建后自动校验：文件大小、GOOS/GOARCH/trimpath（读 buildinfo）、
    本机可执行的目标直接跑 -version 与源码版本比对，最后打印 SHA256。

.PARAMETER Version
    期望版本（如 v0.5.1 或 0.5.1）。只用于断言：与 internal/version/version.go 不一致时报错。

.PARAMETER OS
    目标平台：windows（默认）| linux | all。

.PARAMETER Arch
    目标架构：amd64 | arm64 | all（默认 all）。

.PARAMETER Out
    输出目录，默认 dist（相对仓库根）。

.PARAMETER NoZip
    只产出二进制，不打包 zip。

.PARAMETER ZipAll
    为所有架构打 zip（默认只有 amd64）。

.PARAMETER NoVCS
    传 -buildvcs=false，产物不嵌入 git commit；默认可追溯（vcs.revision/vcs.modified）。

.EXAMPLE
    .\build.ps1
    构建 windows/amd64 + windows/arm64，产出 dist/pairlink-windows-*.exe 与 amd64 的 zip。

.EXAMPLE
    .\build.ps1 -OS all -Version v0.5.1
    一次构建四个平台产物（含 linux），并断言版本号。

.EXAMPLE
    .\build.ps1 -WhatIf
    只打印将执行的命令，不落盘。
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$Version,

    [ValidateSet('windows', 'linux', 'all')]
    [string]$OS = 'windows',

    [ValidateSet('amd64', 'arm64', 'all')]
    [string]$Arch = 'all',

    [string]$Out = 'dist',

    [switch]$NoZip,

    [switch]$ZipAll,

    [switch]$NoVCS
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Step([string]$Message) {
    Write-Host ("==> {0}" -f $Message) -ForegroundColor Cyan
}

function Write-Info([string]$Message) {
    Write-Host ("    {0}" -f $Message)
}

function Fail([string]$Message) {
    throw "构建失败：$Message"
}

function Get-GoSemver([string]$Text) {
    # "go1.24.4" / "go1.25rc1" -> @(1,24,4)；解析失败返回 $null
    if ($Text -notmatch 'go(\d+)\.(\d+)(?:\.(\d+))?') { return $null }
    $patch = if ($Matches[3]) { [int]$Matches[3] } else { 0 }
    return @([int]$Matches[1], [int]$Matches[2], $patch)
}

function Compare-Semver($A, $B) {
    for ($i = 0; $i -lt 3; $i++) {
        if ($A[$i] -lt $B[$i]) { return -1 }
        if ($A[$i] -gt $B[$i]) { return 1 }
    }
    return 0
}

function Get-Sha256([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Format-Size([long]$Bytes) {
    return ('{0:N0} B ({1:N1} MiB)' -f $Bytes, ($Bytes / 1MB))
}

# ---------------------------------------------------------------- 环境与版本
# 脚本就放在仓库根目录，仓库根 = 脚本所在目录（用 Get-Location 兜底 -File 以外的调用方式）
$Root = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
if (-not (Test-Path -LiteralPath (Join-Path $Root 'go.mod'))) {
    Fail "脚本必须放在仓库根目录（找不到 $Root\go.mod）"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Fail '找不到 go 命令：请安装 Go 1.24.4+ 并加入 PATH'
}
Set-Location -LiteralPath $Root

$versionFile = Join-Path $Root 'internal/version/version.go'
if (-not (Test-Path -LiteralPath $versionFile)) { Fail '找不到 internal/version/version.go' }
$versionMatch = Select-String -LiteralPath $versionFile -Pattern '^const Version = "(.+)"$'
if (-not $versionMatch) { Fail "无法从 $versionFile 解析 Version 常量" }
$sourceVersion = $versionMatch.Matches[0].Groups[1].Value
if (-not $sourceVersion) { Fail "无法从 $versionFile 解析 Version 常量" }

if (-not [string]::IsNullOrWhiteSpace($Version)) {
    $want = if ($Version.StartsWith('v')) { $Version } else { 'v' + $Version }
    if ($want -ne $sourceVersion) {
        Fail ("-Version {0} 与 internal/version/version.go 的 {1} 不一致；版本号唯一来源是源码常量" -f $want, $sourceVersion)
    }
    Write-Info ("版本断言通过：{0}" -f $sourceVersion)
}

$goMod = Get-Content -LiteralPath (Join-Path $Root 'go.mod')
$goDirectiveMatch = $goMod | Select-String -Pattern '^go (\d+\.\d+(\.\d+)?)$'
$goDirective = if ($goDirectiveMatch) { $goDirectiveMatch.Matches[0].Groups[1].Value } else { '' }
$goActualRaw = (& go env GOVERSION 2>&1 | Out-String).Trim()
$goActual = Get-GoSemver $goActualRaw
$goRequired = Get-GoSemver ('go' + $goDirective)
if ($null -eq $goActual) { Fail "无法解析 Go 版本：$goActualRaw" }
if ($null -ne $goRequired -and (Compare-Semver $goActual $goRequired) -lt 0) {
    Fail ("需要 Go {0}+，当前 {1}" -f $goDirective, $goActualRaw)
}
Write-Info ("go {0}（go.mod 要求 {1}+），版本常量 {2}" -f $goActualRaw, $goDirective, $sourceVersion)

# git 不可用（非仓库副本、dubious ownership、缺少 git）时 go build 会因 VCS 印章失败，
# 这里探测一次并自动退回 -buildvcs=false，避免"构建莫名其妙失败"。
$gvcs = $true
$head = 'unknown'
$headRaw = (& git rev-parse --short HEAD 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $headRaw -match '^fatal') {
    $gvcs = $false
    if (-not $NoVCS) {
        Write-Warning ("git 不可用（{0}）；自动加 -buildvcs=false（如需 VCS 印章，先解决 git 权限/仓库状态）" -f $headRaw)
        $NoVCS = $true
    }
}
else {
    $head = $headRaw
}
$dirty = $false
if ($gvcs) {
    $dirty = ((& git status --porcelain 2>$null | Out-String).Trim() -ne '')
    if ($dirty) {
        Write-Warning '工作区有未提交改动：产物会记录 vcs.modified=true（加 -NoVCS 可不嵌入 VCS 信息）'
    }
}

$outDir = if ([System.IO.Path]::IsPathRooted($Out)) { $Out } else { Join-Path $Root $Out }
$targets = @()
$osList = if ($OS -eq 'all') { @('windows', 'linux') } else { @($OS) }
$archList = if ($Arch -eq 'all') { @('amd64', 'arm64') } else { @($Arch) }
foreach ($tOs in $osList) { foreach ($tArch in $archList) { $targets += [pscustomobject]@{ OS = $tOs; Arch = $tArch } } }

Write-Host ''
Write-Step ("构建 {0}，输出 {1}" -f (($targets | ForEach-Object { "$($_.OS)/$($_.Arch)" }) -join ' + '), $outDir)
Write-Info ("版本 {0}，HEAD {1}{2}，VCS 嵌入 {3}" -f $sourceVersion, $head, $(if ($dirty) { ' (dirty)' } else { '' }), (-not $NoVCS))

if (-not (Test-Path -LiteralPath $outDir)) {
    if ($PSCmdlet.ShouldProcess($outDir, '创建输出目录')) {
        New-Item -ItemType Directory -Path $outDir -Force | Out-Null
    }
}

# ---------------------------------------------------------------- 逐个目标构建
$results = @()
$index = 0
foreach ($target in $targets) {
    $index++
    $tOs = $target.OS
    $tArch = $target.Arch
    $exeName = if ($tOs -eq 'windows') { 'pairlink.exe' } else { 'pairlink' }
    $artifact = Join-Path $outDir ('pairlink-{0}-{1}{2}' -f $tOs, $tArch, $(if ($tOs -eq 'windows') { '.exe' } else { '' }))
    $packageName = 'pairlink-{0}-{1}-{2}' -f $sourceVersion, $tOs, $tArch

    Write-Host ''
    Write-Step ("[{0}/{1}] {2}/{3} -> {4}" -f $index, $targets.Count, $tOs, $tArch, (Split-Path -Leaf $artifact))

    $buildArgs = @('build', '-trimpath', '-ldflags', '-s -w', '-o', $artifact)
    if ($NoVCS) { $buildArgs += '-buildvcs=false' }
    $buildArgs += '.'

    $oldGoOs = $env:GOOS
    $oldGoArch = $env:GOARCH
    $oldCgo = $env:CGO_ENABLED
    try {
        $env:GOOS = $tOs
        $env:GOARCH = $tArch
        $env:CGO_ENABLED = '0'
        $displayArgs = ($buildArgs | ForEach-Object { if ($_ -match '\s') { '"' + $_ + '"' } else { $_ } }) -join ' '
        Write-Info ("CGO_ENABLED=0 GOOS={0} GOARCH={1} go {2}" -f $tOs, $tArch, $displayArgs)
        if ($PSCmdlet.ShouldProcess($artifact, 'go build')) {
            $buildOutput = & go @buildArgs 2>&1 | Out-String
            if ($LASTEXITCODE -ne 0) { Fail ("go build 退出码 {0}`n{1}" -f $LASTEXITCODE, $buildOutput.Trim()) }
        }
    }
    finally {
        $env:GOOS = $oldGoOs
        $env:GOARCH = $oldGoArch
        $env:CGO_ENABLED = $oldCgo
    }

    if (-not $PSCmdlet.ShouldProcess($artifact, '校验产物')) { continue }

    if (-not (Test-Path -LiteralPath $artifact)) { Fail "产物不存在：$artifact" }
    $size = (Get-Item -LiteralPath $artifact).Length
    if ($size -lt 5MB) { Fail ("产物只有 {0}，疑似构建异常" -f (Format-Size $size)) }

    # buildinfo 校验：平台、trimpath、CGO
    $buildInfo = (& go version -m $artifact 2>&1 | Out-String)
    foreach ($needle in @("GOOS=$tOs", "GOARCH=$tArch", '-trimpath=true', 'CGO_ENABLED=0')) {
        if ($buildInfo -notmatch [regex]::Escape($needle)) {
            Fail ("产物 buildinfo 缺少 {0}`n{1}" -f $needle, $buildInfo.Trim())
        }
    }
    Write-Info 'buildinfo 校验通过（GOOS/GOARCH/trimpath/CGO_ENABLED=0）'

    # 本机可直接执行的目标：跑 -version 与源码版本比对
    # 注意 PowerShell 5.1 没有 $IsWindows/$IsLinux（StrictMode 下引用即异常），改用探测。
    $hostOs = 'unknown'
    if ($env:OS -eq 'Windows_NT') { $hostOs = 'windows' }
    elseif (Test-Path -LiteralPath '/proc/sys/kernel/ostype') { $hostOs = 'linux' }
    elseif (Test-Path -LiteralPath '/System/Library/CoreServices/SystemVersion.plist') { $hostOs = 'darwin' }
    $hostArch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
        'X64' { 'amd64' }
        'Arm64' { 'arm64' }
        'X86' { '386' }
        default { 'unknown' }
    }
    $localVersion = $null
    if ($tOs -eq $hostOs -and $tArch -eq $hostArch) {
        $versionOut = (& $artifact -version 2>&1 | Out-String).Trim()
        $expected = "pairlink $sourceVersion"
        if ($versionOut -ne $expected) {
            Fail ("-version 输出 `"{0}`" 与源码版本 `"{1}`" 不一致" -f $versionOut, $expected)
        }
        $localVersion = $versionOut
        Write-Info ("本机执行 -version = {0}" -f $versionOut)
    }
    else {
        Write-Info ("跨平台目标，跳过本机执行（仅校验 buildinfo）")
    }

    $sha = Get-Sha256 $artifact
    Write-Info ("大小 {0}，SHA256 {1}" -f (Format-Size $size), $sha)

    # ------------------------------------------------------------ 打包
    $zipPath = $null
    $zipSha = $null
    $wantZip = (-not $NoZip) -and (($tArch -eq 'amd64') -or $ZipAll)
    if ($wantZip) {
        $zipPath = Join-Path $outDir ('{0}.zip' -f $packageName)
        $stage = Join-Path $outDir $packageName
        if ($PSCmdlet.ShouldProcess($zipPath, '打包 zip')) {
            if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
            New-Item -ItemType Directory -Path $stage -Force | Out-Null
            Copy-Item -LiteralPath $artifact -Destination (Join-Path $stage $exeName) -Force
            foreach ($item in 'LICENSE', 'README.md') {
                $src = Join-Path $Root $item
                if (Test-Path -LiteralPath $src) { Copy-Item -LiteralPath $src -Destination $stage -Force }
            }
            foreach ($dir in 'deploy', 'examples') {
                $src = Join-Path $Root $dir
                if (Test-Path -LiteralPath $src) { Copy-Item -LiteralPath $src -Destination $stage -Recurse -Force }
            }
            Add-Type -AssemblyName System.IO.Compression.FileSystem
            if (Test-Path -LiteralPath $zipPath) { Remove-Item -LiteralPath $zipPath -Force }
            [System.IO.Compression.ZipFile]::CreateFromDirectory(
                $stage, $zipPath,
                [System.IO.Compression.CompressionLevel]::Optimal,
                $true)  # includeBaseDirectory -> zip 内有 <packageName>/ 顶层目录
            Remove-Item -LiteralPath $stage -Recurse -Force
        }
        if (Test-Path -LiteralPath $zipPath) {
            $zipSha = Get-Sha256 $zipPath
            $entries = 0
            $z = [System.IO.Compression.ZipFile]::OpenRead($zipPath)
            try { $entries = $z.Entries.Count } finally { $z.Dispose() }
            Write-Info ("打包 {0}（{1} 个条目，{2}，SHA256 {3}）" -f (Split-Path -Leaf $zipPath), $entries, (Format-Size (Get-Item $zipPath).Length), $zipSha)
        }
    }
    else {
        Write-Info '跳过打包（-NoZip 或非 amd64 且未加 -ZipAll）'
    }

    $results += [pscustomobject]@{
        Target  = '{0}/{1}' -f $tOs, $tArch
        File    = (Split-Path -Leaf $artifact)
        Size    = $size
        Sha256  = $sha
        Zip     = $zipPath
        Local   = $localVersion
    }
}

# ---------------------------------------------------------------- 汇总
Write-Host ''
Write-Step '汇总'
$results | Format-Table -AutoSize -Property Target, File, @{ N = 'Size'; E = { '{0:N1} MiB' -f ($_.Size / 1MB) } }, @{ N = 'SHA256'; E = { $_.Sha256.Substring(0, 16) + '…' } }, @{ N = 'Version'; E = { if ($_.Local) { $_.Local } else { '(cross)' } } } | Out-String | Write-Host
foreach ($r in $results) {
    Write-Host ("    {0}  sha256={1}" -f $r.File, $r.Sha256)
    if ($r.Zip) { Write-Host ("    {0}  sha256={1}" -f (Split-Path -Leaf $r.Zip), (Get-Sha256 $r.Zip)) }
}
Write-Host ''
Write-Host ("完成：{0} 个目标，输出目录 {1}" -f $targets.Count, $outDir) -ForegroundColor Green