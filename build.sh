#!/usr/bin/env bash
# ============================================================================
# PairLink Linux（默认）与可选跨平台构建脚本
# ----------------------------------------------------------------------------
# 与 build.ps1 行为一致，只是面向 Linux/macOS 主机：
#   * 版本号只读 internal/version/version.go 的 Version 常量（唯一来源），
#     --version 只做断言，不改写任何文件；
#   * 统一构建参数 CGO_ENABLED=0 + -trimpath + -ldflags "-s -w"；
#   * 默认构建 linux/amd64 与 linux/arm64；默认只为 amd64 打 zip；
#   * zip 内布局 <name>/pairlink、LICENSE、README.md、deploy/**、examples/**。
#
# 用法见 --help。
# ============================================================================
set -euo pipefail

DRY_RUN=0
TARGET_OS=linux
TARGET_ARCH=all
OUT_DIR=dist
NO_ZIP=0
ZIP_ALL=0
NO_VCS=0
WANT_VERSION=""

usage() {
    cat <<'USAGE'
用法: ./build.sh [选项]

选项:
  --version <vX.Y.Z>   断言与 internal/version/version.go 一致（只校验，不改写源码）
  --os <linux|windows|all>   默认 linux
  --arch <amd64|arm64|all>   默认 all
  --out <dir>          输出目录，默认 <仓库根>/dist
  --no-zip             只产出二进制
  --zip-all            为所有架构打 zip（默认只有 amd64）
  --no-vcs             传 -buildvcs=false（不嵌入 git commit）
  --dry-run            只打印命令，不落盘
  -h, --help           显示本帮助

示例:
  ./build.sh                         # linux/amd64 + linux/arm64（amd64 打 zip）
  ./build.sh --os all --version v0.5.1
  ./build.sh --arch amd64 --no-zip
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version) WANT_VERSION="${2:?--version 需要一个值}"; shift 2 ;;
        --os) TARGET_OS="${2:?--os 需要一个值}"; shift 2 ;;
        --arch) TARGET_ARCH="${2:?--arch 需要一个值}"; shift 2 ;;
        --out) OUT_DIR="${2:?--out 需要一个值}"; shift 2 ;;
        --no-zip) NO_ZIP=1; shift ;;
        --zip-all) ZIP_ALL=1; shift ;;
        --no-vcs) NO_VCS=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "未知参数: $1（--help 查看用法）" >&2; exit 2 ;;
    esac
done

case "$TARGET_OS" in linux|windows|all) ;; *) echo "--os 只接受 linux/windows/all" >&2; exit 2 ;; esac
case "$TARGET_ARCH" in amd64|arm64|all) ;; *) echo "--arch 只接受 amd64/arm64/all" >&2; exit 2 ;; esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$ROOT/go.mod" ] || { echo "脚本必须放在仓库根目录（找不到 $ROOT/go.mod）" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "找不到 go 命令：请安装 Go 1.24.4+ 并加入 PATH" >&2; exit 1; }
cd "$ROOT"

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die() { printf '构建失败：%s\n' "$*" >&2; exit 1; }

run() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '    [dry-run] %s\n' "$*"
        return 0
    fi
    "$@"
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

file_size() { wc -c < "$1" | tr -d ' '; }

size_human() {
    awk -v b="$1" 'BEGIN { printf "%.1f MiB", b/1048576 }'
}

ver_compare() { # 打印 -1/0/1
    awk -v a="$1" -v b="$2" 'BEGIN {
        n = split(a, x, "."); m = split(b, y, ".");
        for (i = 1; i <= 3; i++) {
            xi = (i <= n) ? x[i] + 0 : 0
            yi = (i <= m) ? y[i] + 0 : 0
            if (xi < yi) { print -1; exit }
            if (xi > yi) { print 1; exit }
        }
        print 0
    }'
}

# ---------------------------------------------------------------- 环境与版本
VERSION_FILE="$ROOT/internal/version/version.go"
[ -f "$VERSION_FILE" ] || die "找不到 internal/version/version.go"
SRC_VERSION="$(sed -nE 's/^const Version = "(.+)"$/\1/p' "$VERSION_FILE" | head -1)"
[ -n "$SRC_VERSION" ] || die "无法从 $VERSION_FILE 解析 Version 常量"

if [ -n "$WANT_VERSION" ]; then
    case "$WANT_VERSION" in v*) want="$WANT_VERSION" ;; *) want="v$WANT_VERSION" ;; esac
    [ "$want" = "$SRC_VERSION" ] || die "--version $want 与源码常量 $SRC_VERSION 不一致（版本号唯一来源是源码）"
    info "版本断言通过：$SRC_VERSION"
fi

GO_DIRECTIVE="$(sed -nE 's/^go ([0-9]+\.[0-9]+(\.[0-9]+)?)$/\1/p' "$ROOT/go.mod" | head -1)"
GO_ACTUAL_RAW="$(go env GOVERSION 2>/dev/null || true)"
GO_ACTUAL="$(printf '%s' "$GO_ACTUAL_RAW" | sed -nE 's/^go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/p')"
[ -n "$GO_ACTUAL" ] || die "无法解析 Go 版本：$GO_ACTUAL_RAW"
if [ -n "$GO_DIRECTIVE" ] && [ "$(ver_compare "$GO_ACTUAL" "$GO_DIRECTIVE")" = "-1" ]; then
    die "需要 Go $GO_DIRECTIVE+，当前 $GO_ACTUAL_RAW"
fi
info "go $GO_ACTUAL_RAW（go.mod 要求 ${GO_DIRECTIVE:-未声明}+），版本常量 $SRC_VERSION"

# git 不可用（非仓库副本、dubious ownership、没装 git）时 go build 会因 VCS 印章失败：
# 探测一次并自动退回 -buildvcs=false，避免"构建莫名其妙失败"。
HEAD_SHORT="$(git rev-parse --short HEAD 2>/dev/null || true)"
if [ -z "$HEAD_SHORT" ]; then
    HEAD_SHORT='unknown'
    if [ "$NO_VCS" = 0 ]; then
        printf '    WARNING: git 不可用（非仓库或权限问题），自动加 -buildvcs=false\n' >&2
        NO_VCS=1
    fi
elif [ -n "$(git status --porcelain 2>/dev/null || true)" ]; then
    printf '    WARNING: 工作区有未提交改动，产物会记录 vcs.modified=true（--no-vcs 可关闭 VCS 嵌入）\n' >&2
fi

case "$OUT_DIR" in
    /*) OUT_ABS="$OUT_DIR" ;;
    *) OUT_ABS="$ROOT/$OUT_DIR" ;;
esac

os_list=(linux)
[ "$TARGET_OS" = all ] && os_list=(windows linux)
[ "$TARGET_OS" = windows ] && os_list=(windows)
arch_list=(amd64 arm64)
[ "$TARGET_ARCH" != all ] && arch_list=("$TARGET_ARCH")

TARGETS=()
for o in "${os_list[@]}"; do for a in "${arch_list[@]}"; do TARGETS+=("$o/$a"); done; done

JOINED="$(printf '%s + ' "${TARGETS[@]}")"; JOINED="${JOINED% + }"
printf '\n==> 构建 %s，输出 %s\n' "$JOINED" "$OUT_ABS"
info "版本 $SRC_VERSION，HEAD $HEAD_SHORT，VCS 嵌入 $([ "$NO_VCS" = 1 ] && echo false || echo true)"

if [ ! -d "$OUT_ABS" ]; then
    run mkdir -p "$OUT_ABS"
fi

# ---------------------------------------------------------------- 逐个目标构建
SUMMARY=()
IDX=0
for t in "${TARGETS[@]}"; do
    IDX=$((IDX + 1))
    t_os="${t%%/*}"
    t_arch="${t##*/}"
    if [ "$t_os" = windows ]; then exe_name='pairlink.exe'; ext='.exe'; else exe_name='pairlink'; ext=''; fi
    artifact="$OUT_ABS/pairlink-$t_os-$t_arch$ext"
    pkg="pairlink-$SRC_VERSION-$t_os-$t_arch"

    step "[$IDX/${#TARGETS[@]}] $t_os/$t_arch -> $(basename "$artifact")"

    build_args=(build -trimpath -ldflags '-s -w' -o "$artifact")
    [ "$NO_VCS" = 1 ] && build_args+=(-buildvcs=false)
    build_args+=(.)

    display="go build -trimpath -ldflags \"-s -w\" -o $artifact"
    [ "$NO_VCS" = 1 ] && display="$display -buildvcs=false"
    info "CGO_ENABLED=0 GOOS=$t_os GOARCH=$t_arch $display"
    run env CGO_ENABLED=0 GOOS="$t_os" GOARCH="$t_arch" go "${build_args[@]}" \
        || die "go build 失败（$t_os/$t_arch）"

    if [ "$DRY_RUN" = 1 ]; then
        SUMMARY+=("$t_os/$t_arch|-|-|-|(dry-run)")
        continue
    fi

    [ -f "$artifact" ] || die "产物不存在：$artifact"
    size="$(file_size "$artifact")"
    [ "$size" -ge 5242880 ] || die "产物只有 $size 字节，疑似构建异常"
    info "大小 $(size_human "$size")"

    build_info="$(go version -m "$artifact" 2>&1)"
    for needle in "GOOS=$t_os" "GOARCH=$t_arch" '-trimpath=true' 'CGO_ENABLED=0'; do
        printf '%s' "$build_info" | grep -qF -- "$needle" || die "产物 buildinfo 缺少 $needle：
$build_info"
    done
    info 'buildinfo 校验通过（GOOS/GOARCH/trimpath/CGO_ENABLED=0）'

    host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    host_arch="$(uname -m)"
    case "$host_arch" in x86_64|amd64) host_arch=amd64 ;; aarch64|arm64) host_arch=arm64 ;; esac
    case "$host_os" in msys*|cygwin*|mingw*) host_os=windows ;; esac

    local_version='(cross)'
    if [ "$t_os" = "$host_os" ] && [ "$t_arch" = "$host_arch" ]; then
        version_out="$(chmod +x "$artifact" && "$artifact" -version 2>&1 | head -1)"
        expected="pairlink $SRC_VERSION"
        [ "$version_out" = "$expected" ] || die "-version 输出 \"$version_out\" 与源码版本 \"$expected\" 不一致"
        local_version="$version_out"
        info "本机执行 -version = $version_out"
    else
        info '跨平台目标，跳过本机执行（仅校验 buildinfo）'
    fi

    sha="$(sha256_of "$artifact")"
    info "SHA256 $sha"

    # ------------------------------------------------------------ 打包
    zip_path=''
    want_zip=1
    [ "$NO_ZIP" = 1 ] && want_zip=0
    [ "$t_arch" != amd64 ] && [ "$ZIP_ALL" != 1 ] && want_zip=0
    if [ "$want_zip" = 1 ]; then
        zip_path="$OUT_ABS/$pkg.zip"
        stage="$OUT_ABS/$pkg"
        info "打包 $(basename "$zip_path")"
        run rm -rf "$stage"
        run mkdir -p "$stage"
        run cp -f "$artifact" "$stage/$exe_name"
        for f in LICENSE README.md; do
            [ -f "$ROOT/$f" ] && run cp -f "$ROOT/$f" "$stage/"
        done
        for d in deploy examples; do
            [ -d "$ROOT/$d" ] && run cp -a "$ROOT/$d" "$stage/"
        done
        run rm -f "$zip_path"
        if command -v zip >/dev/null 2>&1; then
            run bash -c 'cd "$1" && zip -q -r -X "$2" "$3"' _ "$OUT_ABS" "$zip_path" "$pkg"
        elif command -v python3 >/dev/null 2>&1; then
            run python3 -c 'import shutil,sys; shutil.make_archive(sys.argv[1], "zip", root_dir=sys.argv[2], base_dir=sys.argv[3])' \
                "${zip_path%.zip}" "$OUT_ABS" "$pkg"
        else
            die "需要 zip 或 python3 才能打包（或加 --no-zip）"
        fi
        run rm -rf "$stage"
        zip_sha="$(sha256_of "$zip_path")"
        entries="$(python3 -c 'import sys,zipfile; print(len(zipfile.ZipFile(sys.argv[1]).namelist()))' "$zip_path" 2>/dev/null || echo '?')"
        info "zip $(size_human "$(file_size "$zip_path")")，$entries 个条目，SHA256 $zip_sha"
        SUMMARY+=("$t_os/$t_arch|$size|$sha|$zip_path|$local_version")
    else
        info '跳过打包（--no-zip 或非 amd64 且未加 --zip-all）'
        SUMMARY+=("$t_os/$t_arch|$size|$sha|-|$local_version")
    fi
done

# ---------------------------------------------------------------- 汇总
printf '\n==> 汇总\n'
printf '    %-16s %-12s %-14s %s\n' '目标' '大小' '版本' 'SHA256'
for row in "${SUMMARY[@]}"; do
    IFS='|' read -r target size sha _zip local_version <<<"$row"
    if [ "$size" = '-' ]; then
        printf '    %-16s %-12s %-14s %s\n' "$target" '-' '-' "$sha"
        continue
    fi
    printf '    %-16s %-12s %-14s %s\n' "$target" "$(size_human "$size")" "$local_version" "$sha"
done
printf '\n完成：%d 个目标，输出目录 %s\n' "${#TARGETS[@]}" "$OUT_ABS"