#!/usr/bin/env bash
# PairLink Linux 卸载脚本：停止/禁用并移除 systemd unit 与二进制。
#
# 默认**保留** /etc/pairlink（配置、证书、日志排查现场）；只有 --purge 才删除。
# 默认也不删除 pairlink 系统用户/组，避免误删仍被其它服务使用的账号。
set -euo pipefail

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

BIN_DEST=/usr/local/bin/pairlink
ETC_DIR=/etc/pairlink
SERVICE_USER=pairlink
UNIT_DEST=/etc/systemd/system
SYSTEMD_SERVICES=(pairlink-relay.service pairlink-agent.service pairlink-client.service)

PURGE=0
DRY_RUN=0

usage() {
	cat <<'EOF'
用法: uninstall.sh [选项]

停止并 disable 三个 PairLink systemd unit，删除 /etc/systemd/system 下的 unit
与 /usr/local/bin/pairlink。默认保留 /etc/pairlink。

选项:
  --purge    同时删除 /etc/pairlink（配置与证书，不可恢复）。
  --dry-run  只打印将执行的命令，不做任何修改；可非 root。
  -h, --help 显示本帮助。

示例:
  sudo ./deploy/linux/uninstall.sh
  sudo ./deploy/linux/uninstall.sh --purge
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

while [ "$#" -gt 0 ]; do
	case "$1" in
		--purge)
			PURGE=1
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
		warn "当前不是 root：--dry-run 只做预览，不会删除任何文件。"
	else
		die "需要 root 权限：请用 sudo 运行，或先加 --dry-run 预览。"
	fi
fi

# ---- 停止并 disable 服务 ----------------------------------------------------
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	for svc in "${SYSTEMD_SERVICES[@]}"; do
		if systemctl cat "$svc" >/dev/null 2>&1; then
			log "停止并 disable: $svc"
			run systemctl disable --now "$svc" || warn "停止/禁用 $svc 失败，继续卸载（可用 systemctl status $svc 排查）。"
		else
			log "未发现已加载的 unit: $svc（跳过）"
		fi
	done
else
	warn "未检测到运行中的 systemd（缺少 /run/systemd/system），跳过服务停止。"
fi

# ---- 删除 unit 文件 ---------------------------------------------------------
removed_unit=0
for svc in "${SYSTEMD_SERVICES[@]}"; do
	if [ -f "$UNIT_DEST/$svc" ]; then
		run rm -f -- "$UNIT_DEST/$svc"
		removed_unit=1
	fi
done
if [ "$removed_unit" -eq 1 ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	run systemctl daemon-reload
fi

# ---- 删除二进制 -------------------------------------------------------------
if [ -e "$BIN_DEST" ]; then
	log "删除二进制: $BIN_DEST"
	run rm -f -- "$BIN_DEST"
else
	log "二进制不存在，跳过: $BIN_DEST"
fi

# ---- 配置目录 ---------------------------------------------------------------
if [ "$PURGE" -eq 1 ]; then
	[ "$ETC_DIR" = "/etc/pairlink" ] || die "拒绝删除非预期路径: $ETC_DIR"
	if [ -d "$ETC_DIR" ]; then
		log "删除配置目录（--purge）: $ETC_DIR"
		run rm -rf -- "$ETC_DIR"
	else
		log "配置目录不存在，跳过: $ETC_DIR"
	fi
else
	log "保留配置目录: $ETC_DIR（如需一并删除：sudo $SCRIPT_DIR/uninstall.sh --purge）"
fi

cat <<EOF

[pairlink] 卸载完成。
  - 未删除系统用户/组（$SERVICE_USER 若仍被其它服务使用，请手动确认后再 userdel/groupdel）。
  - 如仍需清理日志：journalctl --rotate && journalctl --vacuum-time=1s（会清空所有 unit 日志，慎用）。
EOF
