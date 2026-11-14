#!/usr/bin/env bash
# ============================================================================
# PairLink Linux 部署公共函数库（内部文件，**不是部署入口**）。
# ----------------------------------------------------------------------------
# 由 deploy/linux/install-<mode>.sh 通过 `. common.sh` 引用；只定义函数与只读
# 默认值，不执行任何安装动作。请直接运行 install-relay.sh / install-agent.sh /
# install-client.sh。
#
# 三个模式共用：共享二进制安装/原子替换、服务账号、配置目录、示例 ini、
# unit 安装、systemd 启停、按模式卸载与"共享二进制仅在无残留 unit 时删除"。
# 每个入口脚本只传入自己的 MODE / UNIT_NAME，绝不安装其它模式。
# ============================================================================

# 本文件的全局状态由 install-<mode>.sh 读取；单独分析 common.sh 时关掉误报。
# shellcheck disable=SC2034

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	printf '[pairlink] common.sh 是内部函数库，不是部署入口；请运行 install-<mode>.sh。\n' >&2
	exit 1
fi

BIN_DEST=${BIN_DEST:-/usr/local/bin/pairlink}
ETC_DIR=${ETC_DIR:-/etc/pairlink}
UNIT_DEST=${UNIT_DEST:-/etc/systemd/system}
SERVICE_USER=pairlink
SERVICE_GROUP=pairlink
SERVICE_NAME=${UNIT_NAME:-pairlink}
SERVICE_NAME=${SERVICE_NAME%.service}

# 入口脚本在 source 之后设置的全局状态。
UNINSTALL=0
PURGE=0
DRY_RUN=0
NO_START=0
NO_SYSTEMD=0
BINARY=""
BINARY_CHANGED=0
UNIT_REMOVED=0
CONFIG_GROUP=$SERVICE_GROUP
EXAMPLES_DIR=""

# ---- 输出 ------------------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != "dumb" ]; then
	C_RESET=$'\033[0m'
	C_INFO=$'\033[36m'
	C_OK=$'\033[32m'
	C_WARN=$'\033[33m'
	C_ERR=$'\033[31m'
else
	C_RESET=''
	C_INFO=''
	C_OK=''
	C_WARN=''
	C_ERR=''
fi

log() { printf '%s[pairlink]%s %s\n' "$C_INFO" "$C_RESET" "$*"; }
ok() { printf '%s[pairlink]%s %s\n' "$C_OK" "$C_RESET" "$*"; }
warn() { printf '%s[pairlink] 警告:%s %s\n' "$C_WARN" "$C_RESET" "$*" >&2; }
die() {
	printf '%s[pairlink] 错误:%s %s\n' "$C_ERR" "$C_RESET" "$*" >&2
	exit 1
}

show_cmd() {
	printf '[dry-run]'
	printf ' %q' "$@"
	printf '\n'
}

# 所有会修改系统的命令都必须经过 run：--dry-run 时只打印，不落地。
run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		show_cmd "$@"
	else
		"$@"
	fi
}

# ---- 参数与权限 ------------------------------------------------------------
# 由入口脚本定义 usage() 后调用；支持 --help / --uninstall / --purge 等。
parse_args() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--uninstall)
				UNINSTALL=1
				shift
				;;
			--purge)
				PURGE=1
				shift
				;;
			--dry-run)
				DRY_RUN=1
				shift
				;;
			--no-start)
				NO_START=1
				shift
				;;
			--no-systemd)
				NO_SYSTEMD=1
				shift
				;;
			--binary)
				[ "$#" -ge 2 ] || die "--binary 需要路径参数"
				BINARY=$2
				shift 2
				;;
			--binary=*)
				BINARY=${1#*=}
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
	if [ "$PURGE" -eq 1 ] && [ "$UNINSTALL" -eq 0 ]; then
		die "--purge 只能与 --uninstall 一起使用（它只清理由卸载触发的本模式文件）。"
	fi
}

require_root() {
	if [ "$(id -u)" -eq 0 ]; then
		return 0
	fi
	if [ "$DRY_RUN" -eq 1 ]; then
		warn "当前不是 root：--dry-run 只做预览，不会写入系统目录。"
		return 0
	fi
	die "需要 root 权限：请用 sudo 运行，或先加 --dry-run 预览。"
}

group_exists() {
	if command -v getent >/dev/null 2>&1; then
		getent group "$1" >/dev/null 2>&1
	else
		grep -q "^$1:" /etc/group 2>/dev/null
	fi
}

# 创建 pairlink 系统用户/组；缺少工具时降级为 root:root 并告警。
ensure_service_user() {
	CONFIG_GROUP=$SERVICE_GROUP
	if group_exists "$SERVICE_GROUP"; then
		log "组 $SERVICE_GROUP 已存在，跳过创建。"
	elif [ "$DRY_RUN" -eq 1 ] || command -v groupadd >/dev/null 2>&1; then
		run groupadd --system "$SERVICE_GROUP"
	else
		CONFIG_GROUP=root
		warn "未找到 groupadd，无法创建 $SERVICE_GROUP 组；$ETC_DIR 将归属 root:root（仅 root 可读）。"
	fi

	if id -u "$SERVICE_USER" >/dev/null 2>&1; then
		log "用户 $SERVICE_USER 已存在，跳过创建。"
	elif [ "$DRY_RUN" -eq 1 ] || { command -v useradd >/dev/null 2>&1 && group_exists "$SERVICE_GROUP"; }; then
		local nologin=/usr/sbin/nologin
		[ -x "$nologin" ] || nologin=/sbin/nologin
		[ -x "$nologin" ] || nologin=/bin/false
		run useradd --system --gid "$SERVICE_GROUP" --no-create-home --shell "$nologin" "$SERVICE_USER"
	else
		CONFIG_GROUP=root
		warn "未找到 useradd，无法创建 $SERVICE_USER 用户；$ETC_DIR 将归属 root:root。"
	fi
}

# ---- 二进制与配置 ----------------------------------------------------------
resolve_binary() {
	local candidate
	if [ -n "$BINARY" ]; then
		[ -f "$BINARY" ] || die "二进制不存在或不是普通文件: $BINARY"
		BINARY=$(CDPATH='' cd -- "$(dirname -- "$BINARY")" && pwd)/$(basename -- "$BINARY")
		return 0
	fi
	for candidate in \
		"$SCRIPT_DIR/pairlink" \
		"$REPO_DIR/pairlink" \
		"$REPO_DIR/pairlink-linux" \
		"$SCRIPT_DIR/pairlink-linux"; do
		if [ -f "$candidate" ]; then
			BINARY=$candidate
			BINARY=$(CDPATH='' cd -- "$(dirname -- "$BINARY")" && pwd)/$(basename -- "$BINARY")
			return 0
		fi
	done
	die "未找到 pairlink 二进制：请先 go build -o pairlink .，或用 --binary 指定。"
}

find_examples_dir() {
	local candidate
	for candidate in "$REPO_DIR/examples" "$SCRIPT_DIR/examples"; do
		if [ -d "$candidate" ]; then
			EXAMPLES_DIR=$candidate
			return 0
		fi
	done
	EXAMPLES_DIR=""
	return 1
}

# 内容相同则跳过；不同则先写临时文件再 rename，避免覆盖正在运行的二进制时 ETXTBSY。
install_binary() {
	log "安装二进制: $BINARY -> $BIN_DEST"
	if [ -f "$BIN_DEST" ] && command -v cmp >/dev/null 2>&1 && cmp -s "$BINARY" "$BIN_DEST"; then
		log "已安装的二进制内容相同，跳过复制。"
		BINARY_CHANGED=0
		return 0
	fi
	BINARY_CHANGED=1
	PAIRLINK_TMP="$BIN_DEST.new.$$"
	if [ "$DRY_RUN" -eq 1 ]; then
		show_cmd install -m 0755 "$BINARY" "$PAIRLINK_TMP"
		show_cmd mv -f -- "$PAIRLINK_TMP" "$BIN_DEST"
		return 0
	fi
	trap 'rm -f -- "$PAIRLINK_TMP"' EXIT
	install -m 0755 "$BINARY" "$PAIRLINK_TMP"
	mv -f -- "$PAIRLINK_TMP" "$BIN_DEST"
	trap - EXIT
}

install_config_dir() {
	log "准备配置目录: $ETC_DIR（0750，root:$CONFIG_GROUP）"
	run install -d -m 0750 -o root -g "$CONFIG_GROUP" "$ETC_DIR"
}

# 只安装本模式那一份示例 ini；已存在绝不覆盖。
install_example_ini() {
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
		log "将安装示例配置: $dst（启动前必须替换 token；公网 relay 必须配置 TLS）"
	else
		ok "已安装示例配置: $dst（启动前必须替换 token；公网 relay 必须配置 TLS）"
	fi
}

install_unit() {
	local src=$1
	local dst=$2
	[ -f "$src" ] || die "缺少 unit 文件: $src"
	log "安装本模式 unit: $src -> $dst"
	run install -m 0644 "$src" "$dst"
}

# ---- systemd ---------------------------------------------------------------
systemd_running() {
	command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]
}

# enable 后默认 start；--no-start 只 enable。
start_unit() {
	if [ "$NO_START" -eq 1 ]; then
		log "已按 --no-start 跳过启动（unit 仍已 enable）。"
		return 0
	fi
	if [ "$DRY_RUN" -eq 1 ]; then
		log "将启动服务: $SERVICE_NAME"
		show_cmd systemctl start "$SERVICE_NAME"
		return 0
	fi
	local was_active=0
	if [ "$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)" = "active" ]; then
		was_active=1
	fi
	log "启动服务: $SERVICE_NAME"
	if ! systemctl start "$SERVICE_NAME"; then
		warn "启动 $SERVICE_NAME 失败。排查："
		warn "  systemctl status $SERVICE_NAME --no-pager"
		warn "  journalctl -u $SERVICE_NAME -n 50 --no-pager"
		return 1
	fi
	ok "服务已启动: $SERVICE_NAME"
	if [ "$was_active" -eq 1 ] && [ "$BINARY_CHANGED" -eq 1 ]; then
		log "提示：共享二进制已更新；正在运行的服务需 systemctl restart $SERVICE_NAME 才会用上新版本。"
	fi
}

# 返回除本脚本 UNIT_NAME 之外仍存在的 pairlink-*.service（unit 文件、wants
# 符号链接或 systemd 已加载列表）。用于卸载时决定共享二进制能否删除。
find_residual_units() {
	local dir path name
	{
		for dir in "$UNIT_DEST" /run/systemd/system /usr/lib/systemd/system /lib/systemd/system; do
			[ -d "$dir" ] || continue
			for path in "$dir"/pairlink-*.service; do
				[ -e "$path" ] || continue
				printf '%s\n' "$(basename -- "$path")"
			done
			for path in "$dir"/*.wants/pairlink-*.service; do
				[ -e "$path" ] || [ -L "$path" ] || continue
				printf '%s\n' "$(basename -- "$path")"
			done
		done
		if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
			systemctl list-unit-files --type=service --no-legend 'pairlink-*.service' 2>/dev/null |
				awk '{print $1}' || true
		fi
	} | sort -u | grep -v -x -F "$UNIT_NAME" || true
}

# 卸载本模式 unit：disable --now（存在才做）→ 删除 unit 与 wants 链接 → daemon-reload。
uninstall_unit() {
	UNIT_REMOVED=0
	if ! systemd_running; then
		warn "未检测到运行中的 systemd（缺少 /run/systemd/system）：只删除 unit 文件，跳过停服。"
	fi
	if systemd_running && systemctl cat "$UNIT_NAME" >/dev/null 2>&1; then
		log "停止并 disable: $SERVICE_NAME"
		run systemctl disable --now "$SERVICE_NAME" ||
			warn "停止/禁用 $SERVICE_NAME 失败，继续卸载（可用 systemctl status $SERVICE_NAME 排查）。"
	else
		log "未发现已加载的 unit: $SERVICE_NAME（跳过停止）"
	fi

	if [ -f "$UNIT_DEST/$UNIT_NAME" ]; then
		log "删除 unit 文件: $UNIT_DEST/$UNIT_NAME"
		run rm -f -- "$UNIT_DEST/$UNIT_NAME"
		UNIT_REMOVED=1
	else
		log "unit 文件不存在，跳过: $UNIT_DEST/$UNIT_NAME"
	fi

	local link
	for link in "$UNIT_DEST"/*.wants/"$UNIT_NAME"; do
		if [ -L "$link" ]; then
			log "删除 enable 残留链接: $link"
			run rm -f -- "$link"
			UNIT_REMOVED=1
		fi
	done

	if [ "$UNIT_REMOVED" -eq 1 ] && systemd_running; then
		run systemctl daemon-reload
	fi
}

# 共享二进制只在没有任何其它 pairlink-*.service 残留时删除。
remove_binary_if_unused() {
	local residual name
	residual=$(find_residual_units)
	if [ -n "$residual" ]; then
		log "检测到其它 PairLink unit 仍在，保留共享二进制: $BIN_DEST"
		while IFS= read -r name; do
			[ -n "$name" ] && log "  残留 unit: $name"
		done <<<"$residual"
		return 0
	fi
	if [ ! -e "$BIN_DEST" ]; then
		log "二进制不存在，跳过: $BIN_DEST"
		return 0
	fi
	log "没有其它 pairlink-*.service 残留，删除共享二进制: $BIN_DEST"
	run rm -f -- "$BIN_DEST"
}

# --purge：只删除本模式的 ini 与"明确属于该模式"的证书，不碰其它模式。
purge_mode_files() {
	local ini="$ETC_DIR/$MODE.ini"
	if [ -e "$ini" ]; then
		log "删除本模式配置（--purge）: $ini"
		run rm -f -- "$ini"
	else
		log "本模式配置不存在，跳过: $ini"
	fi

	local tls_dir="$ETC_DIR/tls"
	local cert
	for cert in \
		"$tls_dir/$MODE.crt" \
		"$tls_dir/$MODE.key" \
		"$tls_dir/$MODE.pem" \
		"$tls_dir/$MODE-ca.crt" \
		"$tls_dir/$MODE-ca.key"; do
		if [ -e "$cert" ]; then
			log "删除本模式证书（--purge）: $cert"
			run rm -f -- "$cert"
		fi
	done
	if [ -d "$tls_dir/$MODE" ]; then
		log "删除本模式证书目录（--purge）: $tls_dir/$MODE"
		run rm -rf -- "$tls_dir/$MODE"
	fi
}

# 卸载结束提示（各入口共用；只描述本模式）。
print_uninstall_summary() {
	cat <<EOF

[pairlink] $MODE 卸载完成。
  - 只移除了 $UNIT_NAME；其它模式的 unit 与 ini 未被触碰。
  - 共享二进制 $BIN_DEST 仅在没有任何 pairlink-*.service 残留时删除（见上方日志）。
  - 默认保留 $ETC_DIR 与系统用户/组 $SERVICE_USER；
    --purge 只删除本模式的 $MODE.ini 与明确属于 $MODE 的证书。
EOF
}
