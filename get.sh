#!/usr/bin/env bash
# frp-mini 远端一键接入: https://<SERVER>/<name>/ -> 本机 127.0.0.1:<LOCAL>
# 基于纯 WebSocket 控制通道，无需 OpenSSH；可选同时开启公网 TCP 端口转发。
#
# 用法:
#   curl -fsSL https://<SERVER>/get.sh | bash
#   curl -fsSL https://<SERVER>/get.sh | NAME=colab1 bash
#   curl -fsSL https://<SERVER>/get.sh | TCP=1 bash                  # 同时开放 TCP
#   curl -fsSL https://<SERVER>/get.sh | TCP=1 TCP_TARGET=127.0.0.1:22 bash
#   curl -fsSL https://<SERVER>/get.sh | TCP=1 TCP_PORT=16022 bash   # 指定公网端口
set -euo pipefail

SERVER="${SERVER:-dev.cxu.lol}"
LOCAL="${LOCAL:-43110}"

# 允许 SERVER 带协议前缀(便于本地调试 / 纯 HTTP 部署)
if [[ "$SERVER" == http://* || "$SERVER" == https://* ]]; then
  BASE_URL="${SERVER%/}"
  HOSTPORT="${SERVER#*://}"
  [[ "$SERVER" == https://* ]] && WS_SCHEME="wss" || WS_SCHEME="ws"
else
  BASE_URL="https://${SERVER}"
  HOSTPORT="${SERVER}"
  WS_SCHEME="wss"
fi
# TCP=1 开启公网 TCP 转发; TCP_TARGET 为本地目标; TCP_PORT=0 表示由服务端自动分配
TCP="${TCP:-0}"
TCP_TARGET="${TCP_TARGET:-127.0.0.1:${LOCAL}}"
TCP_PORT="${TCP_PORT:-0}"

echo "=================================================="
echo "🚀 正在建立 frp-mini (远端 -> ${SERVER}) 反代接入..."
echo "=================================================="

# 1. 自动生成动态机器名: colab-月日-时分-随机4位 (如 colab-1009-1836-a3f9)
if [ -z "${NAME:-}" ]; then
  DATE_STR=$(date +%m%d-%H%M)
  RAND_STR=$(openssl rand -hex 2 2>/dev/null || echo "$(( $RANDOM % 9000 + 1000 ))")
  NAME="colab-${DATE_STR}-${RAND_STR}"
fi
echo "[1/3] 注册节点名: ${NAME}"

# 2. 检查或自动安装 Bun 运行时 (Colab/Linux 环境下仅需 2-3 秒)
if ! command -v bun >/dev/null 2>&1 && [ ! -x "$HOME/.bun/bin/bun" ]; then
  echo "[2/3] 安装轻量 Bun 运行时..."
  curl -fsSL https://bun.sh/install | bash >/dev/null 2>&1 || true
fi
export PATH="$HOME/.bun/bin:$PATH"

if ! command -v bun >/dev/null 2>&1; then
  echo "❌ Bun 安装失败，请检查网络"
  exit 1
fi

# 3. 获取客户端脚本
mkdir -p /tmp/frp
curl -fsSL -m 15 "${BASE_URL}/client.ts" -o /tmp/frp/client.ts 2>/dev/null || curl -fsSL -m 15 "http://${HOSTPORT}/client.ts" -o /tmp/frp/client.ts

# 4. 后台守护启动 WebSocket 隧道 (可选附带 TCP 端口转发)
pkill -f "bun.*client.ts.*${NAME}" 2>/dev/null || true

TCP_ARGS=()
if [ "$TCP" = "1" ] || [ "$TCP" = "true" ]; then
  TCP_ARGS+=(--tcp-target "$TCP_TARGET")
  if [ "$TCP_PORT" != "0" ]; then
    TCP_ARGS+=(--tcp-port "$TCP_PORT")
  fi
  echo "[3/3] 已开启 TCP 转发: ${TCP_TARGET} -> 公网端口 ${TCP_PORT}(0=自动)"
fi

nohup bun /tmp/frp/client.ts \
  --server "${WS_SCHEME}://${HOSTPORT}/__tunnel" \
  --name "$NAME" \
  --upstream "http://127.0.0.1:${LOCAL}" \
  ${TCP_ARGS[@]+"${TCP_ARGS[@]}"} > /tmp/ws_tunnel.log 2>&1 &
TUNNEL_PID=$!
sleep 2

PUBLIC_URL="${BASE_URL}/${NAME}/"
echo ""
echo "=================================================="
echo "✅ 隧道已在后台成功启动！(PID: $TUNNEL_PID)"
echo "👉 请在浏览器中打开: $PUBLIC_URL"
if [ "$TCP" = "1" ] || [ "$TCP" = "true" ]; then
  TCP_ADDR=$(grep -o 'tcp public: [^ ]*' /tmp/ws_tunnel.log 2>/dev/null | tail -n1 | sed 's/^tcp public: //' || true)
  if [ -n "$TCP_ADDR" ]; then
    echo "👉 TCP 直连地址: ${TCP_ADDR}"
  else
    echo "⚠️  暂未读取到 TCP 端口，请查看 /tmp/ws_tunnel.log"
  fi
fi
echo "=================================================="
echo ""
