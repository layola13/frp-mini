#!/usr/bin/env bash
# Codex Ultra 远端自动反代接入: http://dev.cxu.lol/<name>/ -> 本机 127.0.0.1:43110
# 基于纯 WebSocket 隧道，无需 OpenSSH，不占用系统端口与多余进程
set -euo pipefail

SERVER="${SERVER:-dev.cxu.lol}"
LOCAL="${LOCAL:-43110}"

echo "=================================================="
echo "🚀 正在建立 Codex Ultra (远端 -> ${SERVER}) 反代接入..."
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
curl -fsSL -m 15 "http://${SERVER}/client.ts" -o /tmp/frp/client.ts

# 4. 后台守护启动 WebSocket 隧道
pkill -f "bun.*client.ts.*${NAME}" 2>/dev/null || true

nohup bun /tmp/frp/client.ts \
  --server "ws://${SERVER}/__tunnel" \
  --name "$NAME" \
  --upstream "http://127.0.0.1:${LOCAL}" > /tmp/ws_tunnel.log 2>&1 &
TUNNEL_PID=$!
sleep 2

PUBLIC_URL="http://${SERVER}/${NAME}/"
echo ""
echo "=================================================="
echo "✅ 隧道已在后台成功启动！(PID: $TUNNEL_PID)"
echo "👉 请在浏览器中打开: $PUBLIC_URL"
echo "=================================================="
echo ""
