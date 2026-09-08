#!/usr/bin/env bash
# revproxy 一键安装脚本（Linux + systemd）
# 用法: sudo ./deploy/install.sh [配置目录]  (默认 /etc/revproxy)
set -euo pipefail

CONF_DIR="${1:-/etc/revproxy}"
BIN="/usr/local/bin/revproxy"
SVC="/etc/systemd/system/revproxy.service"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

info() { printf '\033[36m[info]\033[0m %s\n' "$*"; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请用 root 或 sudo 运行"
command -v systemctl >/dev/null || die "未检测到 systemd"

info "编译 revproxy…"
command -v go >/dev/null && GO=go || GO=/usr/local/go/bin/go
[ -x "$GO" ] || command -v go >/dev/null || die "未找到 Go 编译器，请先安装 Go 1.21+ 或手动下载二进制"
(cd "$REPO_DIR" && CGO_ENABLED=0 $GO build -trimpath -ldflags="-s -w" -o /tmp/revproxy-build .)

info "安装二进制到 $BIN"
install -m 0755 /tmp/revproxy-build "$BIN"
rm -f /tmp/revproxy-build

info "准备配置目录 $CONF_DIR"
mkdir -p "$CONF_DIR"
if [ ! -f "$CONF_DIR/config.json" ]; then
  if [ -f "$CONF_DIR/config.json.bak" ]; then
    cp "$CONF_DIR/config.json.bak" "$CONF_DIR/config.json"
  else
    install -m 0600 "$REPO_DIR/examples/config.example.json" "$CONF_DIR/config.json"
    info "已写入示例配置（含演示路由，记得按需修改）"
  fi
fi

info "安装 systemd 服务"
sed "s#/etc/revproxy#$CONF_DIR#g" "$REPO_DIR/deploy/revproxy.service" > "$SVC"
systemctl daemon-reload
systemctl enable revproxy >/dev/null 2>&1 || true
systemctl restart revproxy
sleep 1

info "服务状态："
systemctl status revproxy --no-pager | head -n 12 || true
echo
info "管理后台：http://$(hostname -I 2>/dev/null | awk '{print $1}'):$(grep -o '"addr": *"[^"]*"' "$CONF_DIR/config.json" 2>/dev/null | head -1 | grep -o '[0-9]\+' || echo 8080)/__admin/"
info "查看日志：journalctl -u revproxy -f"
