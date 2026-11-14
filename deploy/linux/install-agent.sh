#!/usr/bin/env bash
# PairLink Linux agent 安装/卸载脚本。
#
# 只处理 agent 一个角色：共享二进制 /usr/local/bin/pairlink、
# /etc/pairlink/agent.ini、/etc/systemd/system/pairlink-agent.service。
# **不会安装或启动 relay/client**：一台机器通常只部署一个角色。
#
# 设计原则：
#   - 幂等：重复执行安全；已有配置绝不覆盖；二进制内容相同则跳过。
#   - 可演练：--dry-run 只打印将执行的命令，不落任何文件。
#   - 共享二进制：卸载时只有确认没有其它 pairlink-*.service 残留才删除。
set -euo pipefail

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/../.." && pwd)
MODE=agent
UNIT_NAME="pairlink-$MODE.service"
UNITS_DIR="$SCRIPT_DIR/../systemd"
if [ -d "$UNITS_DIR" ]; then
	UNITS_DIR=$(CDPATH='' cd -- "$UNITS_DIR" && pwd)
fi

# shellcheck source-path=SCRIPTDIR
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

usage() {
	cat <<'EOF'
用法: install-agent.sh [选项]

只安装 agent：共享二进制 /usr/local/bin/pairlink、/etc/pairlink/agent.ini
（仅缺失时安装）、pairlink-agent.service。不会安装或启动 relay/client。
需要 root 权限（--dry-run 除外，可非 root 预览）。

选项:
  --binary <path>  指定 pairlink 二进制；默认依次查找脚本同目录、仓库根目录的
                   pairlink / pairlink-linux。
  --no-start       安装并 enable，但不立即 start。
  --no-systemd     只放二进制与配置，不安装 unit。
  --dry-run        只打印将执行的命令，不做任何修改；可非 root。
  --uninstall      停止/disable 并删除 agent 的 unit；无其它残留 unit 时删除共享二进制。
  --purge          仅与 --uninstall 同用：额外删除 /etc/pairlink/agent.ini 与
                   明确属于 agent 的证书（不删其它模式的 ini）。
  -h, --help       显示本帮助。

示例:
  sudo ./deploy/linux/install-agent.sh --binary ./pairlink
  sudo ./deploy/linux/install-agent.sh --no-start
  sudo ./deploy/linux/install-agent.sh --uninstall [--purge]
EOF
}

parse_args "$@"
require_root

# ---- 卸载 -------------------------------------------------------------------
if [ "$UNINSTALL" -eq 1 ]; then
	if [ "$NO_START" -eq 1 ] || [ "$NO_SYSTEMD" -eq 1 ]; then
		warn "--uninstall 时忽略 --no-start / --no-systemd。"
	fi
	uninstall_unit
	if [ "$PURGE" -eq 1 ]; then
		purge_mode_files
	else
		log "保留配置: $ETC_DIR/agent.ini（如需删除：--uninstall --purge）"
	fi
	remove_binary_if_unused
	print_uninstall_summary
	exit 0
fi

# ---- 安装 -------------------------------------------------------------------
resolve_binary
if [ "$NO_SYSTEMD" -eq 0 ]; then
	[ -f "$UNITS_DIR/$UNIT_NAME" ] || die "缺少 unit 文件: $UNITS_DIR/$UNIT_NAME"
fi

ensure_service_user
install_binary
install_config_dir
if find_examples_dir; then
	install_example_ini "$EXAMPLES_DIR/agent.ini" "$ETC_DIR/agent.ini"
else
	warn "未找到 examples 目录，跳过示例 ini 安装。"
fi

if [ "$NO_SYSTEMD" -eq 1 ]; then
	log "已按 --no-systemd 跳过 unit 安装（二进制与 agent.ini 已就绪）。"
else
	install_unit "$UNITS_DIR/$UNIT_NAME" "$UNIT_DEST/$UNIT_NAME"
	if systemd_running; then
		log "重新加载 systemd 配置"
		run systemctl daemon-reload
		log "enable 开机自启: $SERVICE_NAME"
		if ! run systemctl enable "$SERVICE_NAME"; then
			die "enable $SERVICE_NAME 失败"
		fi
		if ! start_unit; then
			exit 1
		fi
	else
		warn "未检测到运行中的 systemd（缺少 /run/systemd/system）：unit 已复制，但没有 daemon-reload/enable/start。"
	fi
fi

# ---- 结束提示 ---------------------------------------------------------------
cat <<EOF

[pairlink] agent 安装完成。后续步骤：
  1. 编辑 $ETC_DIR/agent.ini：
       - relay_addr 指向 relay 的隧道地址（公网示例 relay.example.com:7000）；
       - token 必须与 relay 的 auth_token 完全一致并替换占位符；
       - local / service.<ID> 按本机真实服务修改；relay 用私有 CA 时配 tls_ca。
  2. 前台调试（先确认配置能启动）：
       pairlink -agent -config $ETC_DIR/agent.ini
EOF
if [ "$NO_SYSTEMD" -eq 0 ] && systemd_running; then
	cat <<EOF
  3. systemd 托管与排错：
       systemctl status $SERVICE_NAME --no-pager
       journalctl -u $SERVICE_NAME -f
       systemctl restart $SERVICE_NAME        # 改配置后重启
EOF
else
	cat <<EOF
  3. 未安装 systemd unit。需要时手动安装：
       sudo install -m 0644 $UNITS_DIR/$UNIT_NAME $UNIT_DEST/$UNIT_NAME
       sudo systemctl daemon-reload
       sudo systemctl enable --now $SERVICE_NAME
     排错：systemctl status $SERVICE_NAME --no-pager / journalctl -u $SERVICE_NAME -f
EOF
fi
cat <<EOF
  4. 防火墙：agent 只主动出站到 relay，一般无需入站规则；
     需放行出站 7000/tcp、7001/tcp（原生 UDP 再加 7002/udp，按 ini 实际端口调整）。
  5. 卸载：sudo $SCRIPT_DIR/install-agent.sh --uninstall [--purge]
EOF
