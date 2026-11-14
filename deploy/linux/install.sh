#!/usr/bin/env bash
# PairLink Linux 安装脚本：安装二进制、配置目录与示例 ini，可选安装 systemd unit。
#
# 设计原则：
#   - 幂等：重复执行安全；已有配置**绝不覆盖**；二进制内容相同则跳过。
#   - 可演练：--dry-run 只打印将要执行的命令，不落任何文件。
#   - 最小权限：/etc/pairlink 归属 root:pairlink、目录 0750、配置 0640；
#     服务单元默认仍以 root 运行，按需在 unit 中启用 User=pairlink（见 deploy/systemd/README.md）。
set -euo pipefail

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/../.." && pwd)

BIN_DEST=/usr/local/bin/pairlink
ETC_DIR=/etc/pairlink
UNIT_DEST=/etc/systemd/system
SERVICE_USER=pairlink
SERVICE_GROUP=pairlink
SYSTEMD_SERVICES=(pairlink-relay.service pairlink-agent.service pairlink-client.service)

BINARY=""
WITH_SYSTEMD=0
DRY_RUN=0

usage() {
	cat <<'EOF'
用法: install.sh [选项]

安装 PairLink 到 /usr/local/bin/pairlink，并准备 /etc/pairlink 配置目录。
需要 root 权限（--dry-run 除外，可非 root 预览）。

选项:
  --binary <path>  指定要安装的 pairlink 二进制；默认依次查找
                   本脚本同目录、仓库根目录的 pairlink / pairlink-linux。
  --systemd        安装三个 systemd unit 到 /etc/systemd/system 并 enable
                   （不自动 start；单机演示才建议三个都 enable）。
  --dry-run        只打印将执行的命令，不做任何修改；可非 root。
  -h, --help       显示本帮助。

示例:
  sudo ./deploy/linux/install.sh --binary ./pairlink --systemd
  ./deploy/linux/install.sh --dry-run
EOF
}

log() { printf '[pairlink] %s\n' "$*"; }
warn() { printf '[pairlink] 警告: %s\n' "$*" >&2; }
die() { printf '[pairlink] 错误: %s\n' "$*" >&2; exit 1; }

show_cmd() {
	printf '[dry-run]'
	printf ' %q' "$@"
	printf '\n'
}

run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		show_cmd "$@"
	else
		"$@"
	fi
}

group_exists() {
	if command -v getent >/dev/null 2>&1; then
		getent group "$1" >/dev/null 2>&1
	else
		grep -q "^$1:" /etc/group 2>/dev/null
	fi
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--binary)
			[ "$#" -ge 2 ] || die "--binary 需要路径参数"
			BINARY=$2
			shift 2
			;;
		--binary=*)
			BINARY=${1#*=}
			shift
			;;
		--systemd)
			WITH_SYSTEMD=1
			shift
			;;
		--dry-run)
			DRY_RUN=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			die "未知参数: $1（用 --help 查看用法）"
			;;
	esac
done

if [ "$(id -u)" -ne 0 ]; then
	if [ "$DRY_RUN" -eq 1 ]; then
		warn "当前不是 root：--dry-run 只做预览，不会写入系统目录。"
	else
		die "需要 root 权限：请用 sudo 运行，或先加 --dry-run 预览。"
	fi
fi

# ---- 定位二进制 -------------------------------------------------------------
if [ -z "$BINARY" ]; then
	for candidate in \
		"$SCRIPT_DIR/pairlink" \
		"$REPO_DIR/pairlink" \
		"$REPO_DIR/pairlink-linux" \
		"$SCRIPT_DIR/pairlink-linux"; do
		if [ -f "$candidate" ]; then
			BINARY=$candidate
			break
		fi
	done
fi
[ -n "$BINARY" ] || die "未找到 pairlink 二进制：请先 go build -o pairlink .，或用 --binary 指定。"
[ -f "$BINARY" ] || die "二进制不存在或不是普通文件: $BINARY"
BINARY=$(CDPATH='' cd -- "$(dirname -- "$BINARY")" && pwd)/$(basename -- "$BINARY")

# ---- 定位示例配置与 unit ----------------------------------------------------
EXAMPLES_DIR=""
for candidate in "$REPO_DIR/examples" "$SCRIPT_DIR/examples"; do
	if [ -d "$candidate" ]; then
		EXAMPLES_DIR=$candidate
		break
	fi
done
[ -n "$EXAMPLES_DIR" ] || warn "未找到 examples 目录，将跳过示例 ini 安装。"

UNITS_DIR="$SCRIPT_DIR/../systemd"
if [ "$WITH_SYSTEMD" -eq 1 ]; then
	[ -d "$UNITS_DIR" ] || die "未找到 systemd unit 目录: $UNITS_DIR"
	for unit in "${SYSTEMD_SERVICES[@]}"; do
		[ -f "$UNITS_DIR/$unit" ] || die "缺少 unit 文件: $UNITS_DIR/$unit"
	done
fi

# ---- 服务账号 ---------------------------------------------------------------
CONFIG_GROUP=$SERVICE_GROUP
if ! group_exists "$SERVICE_GROUP"; then
	if [ "$DRY_RUN" -eq 1 ] || command -v groupadd >/dev/null 2>&1; then
		run groupadd --system "$SERVICE_GROUP"
	else
		CONFIG_GROUP=root
		warn "未找到 groupadd，无法创建 $SERVICE_GROUP 组；/etc/pairlink 将归属 root:root（仅 root 可读）。"
	fi
elif [ "$DRY_RUN" -eq 1 ]; then
	log "组 $SERVICE_GROUP 已存在，跳过创建。"
fi

if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
	if [ "$DRY_RUN" -eq 1 ] || { command -v useradd >/dev/null 2>&1 && group_exists "$SERVICE_GROUP"; }; then
		NOLOGIN=/usr/sbin/nologin
		if [ ! -x "$NOLOGIN" ]; then
			NOLOGIN=/sbin/nologin
		fi
		if [ ! -x "$NOLOGIN" ]; then
			NOLOGIN=/bin/false
		fi
		run useradd --system --gid "$SERVICE_GROUP" --no-create-home --shell "$NOLOGIN" "$SERVICE_USER"
	else
		CONFIG_GROUP=root
		warn "未找到 useradd，无法创建 $SERVICE_USER 用户；/etc/pairlink 将归属 root:root。"
	fi
elif [ "$DRY_RUN" -eq 1 ]; then
	log "用户 $SERVICE_USER 已存在，跳过创建。"
fi

# ---- 安装二进制 -------------------------------------------------------------
log "安装二进制: $BINARY -> $BIN_DEST"
if [ -f "$BIN_DEST" ] && command -v cmp >/dev/null 2>&1 && cmp -s "$BINARY" "$BIN_DEST"; then
	log "已安装的二进制内容相同，跳过复制。"
else
	# 先写临时文件再 rename：正在运行的二进制可以直接被替换（避免 ETXTBSY）。
	tmp="$BIN_DEST.new.$$"
	trap 'rm -f -- "$tmp"' EXIT
	run install -m 0755 "$BINARY" "$tmp"
	run mv -f -- "$tmp" "$BIN_DEST"
	trap - EXIT
fi

# ---- 配置目录与示例 ini -----------------------------------------------------
log "准备配置目录: $ETC_DIR（0750，root:$CONFIG_GROUP）"
run install -d -m 0750 -o root -g "$CONFIG_GROUP" "$ETC_DIR"

install_example() {
	local src=$1
	local dst=$2
	if [ -e "$dst" ]; then
		log "保留已有配置，跳过: $dst"
		return 0
	fi
	if [ ! -f "$src" ]; then
		warn "示例配置不存在，跳过: $src"
		return 0
	fi
	run install -m 0640 -o root -g "$CONFIG_GROUP" "$src" "$dst"
	if [ "$DRY_RUN" -eq 1 ]; then
		log "将安装示例配置: $dst（启动前必须修改 token；公网 relay 必须配置 TLS）"
	else
		log "已安装示例配置: $dst（启动前必须修改 token；公网 relay 必须配置 TLS）"
	fi
}

if [ -n "$EXAMPLES_DIR" ]; then
	for name in relay.ini agent.ini client.ini; do
		install_example "$EXAMPLES_DIR/$name" "$ETC_DIR/$name"
	done
fi

# ---- systemd ----------------------------------------------------------------
if [ "$WITH_SYSTEMD" -eq 1 ]; then
	log "安装 systemd unit 到 $UNIT_DEST"
	for unit in "${SYSTEMD_SERVICES[@]}"; do
		run install -m 0644 "$UNITS_DIR/$unit" "$UNIT_DEST/$unit"
	done
	if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
		run systemctl daemon-reload
		run systemctl enable "${SYSTEMD_SERVICES[@]}"
		warn "已 enable 三个 unit（适合单机一体化/演示）。生产请按角色 disable 不需要的服务："
		warn "  sudo systemctl disable pairlink-agent pairlink-client"
	else
		warn "未检测到运行中的 systemd（缺少 /run/systemd/system）；unit 已复制，但未 daemon-reload/enable。"
	fi
fi

# ---- 结束提示 ---------------------------------------------------------------
cat <<EOF

[pairlink] 安装完成。后续步骤：
  1. 编辑配置并替换占位符（CHANGE-ME-RELAY-AUTH-TOKEN）：
       $ETC_DIR/relay.ini / agent.ini / client.ini
     公网 relay 必须配置 tls_cert/tls_key，agent/client 配置 tls_ca（见文件内注释）。
  2. 前台调试（先确认配置能启动）：
       pairlink -relay  -config $ETC_DIR/relay.ini
       pairlink -agent  -config $ETC_DIR/agent.ini
       pairlink -client -config $ETC_DIR/client.ini
  3. systemd 托管：
       sudo systemctl start pairlink-relay
       sudo systemctl status pairlink-relay --no-pager
       sudo journalctl -u pairlink-relay -f
EOF
if [ "$WITH_SYSTEMD" -eq 0 ]; then
	cat <<EOF
     如需安装 unit：sudo $SCRIPT_DIR/install.sh --systemd
EOF
fi
cat <<EOF
  4. 防火墙按角色放行（relay: 7000/tcp、7001/tcp、7002/udp；管理页只开给本机）。
  5. 卸载：sudo $SCRIPT_DIR/uninstall.sh [--purge]
EOF
