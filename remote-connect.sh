#!/usr/bin/env bash
# 远端一键接入: https://us.cxu.lol/<name> -> 本机 127.0.0.1:43110
# 用法: NAME=colab1 SERVER=us.cxu.lol USER=tunnel PORT=14000 ./remote-connect.sh
# PORT 是服务端 /__admin/alloc 返回的端口
set -euo pipefail
NAME="${NAME:-colab1}"
SERVER="${SERVER:-us.cxu.lol}"
USER="${USER:-tunnel}"
PORT="${PORT:-14000}"
LOCAL="${LOCAL:-43110}"
KEY="${KEY:-$HOME/.ssh/tunnel}"

if [ ! -f "$KEY" ]; then
  echo "[1/3] 生成密钥 $KEY"
  ssh-keygen -t ed25519 -f "$KEY" -N "" -C "$NAME"
  echo "请把下面公钥发给管理员追加到服务端:"
  cat "$KEY.pub"
  read -rp "已让管理员添加公钥? 回车继续 [Enter] " _
else
  echo "[1/3] 已有密钥 $KEY"
fi

echo "[2/3] 测试 ssh 连通性"
ssh -i "$KEY" -o ConnectTimeout=8 -o BatchMode=yes "$USER@$SERVER" true 2>&1 || {
  echo "注意: 直连返回非0是正常的(tunnel用户ForceCommand), 只要不是 timeout/拒绝连接即可"
}

echo "[3/3] 建立隧道: 远端127.0.0.1:$LOCAL -> 服务端127.0.0.1:$PORT -> https://$SERVER/$NAME/"
if command -v autossh >/dev/null 2>&1; then
  exec autossh -M0 -NTR "127.0.0.1:$PORT:127.0.0.1:$LOCAL" -i "$KEY" \
    -o ServerAliveInterval=15 -o ServerAliveCountMax=3 "$USER@$SERVER"
else
  echo "未装 autossh, 用 ssh 直连(断线需手动重跑, 可加 while true)"
  exec ssh -NTR "127.0.0.1:$PORT:127.0.0.1:$LOCAL" -i "$KEY" \
    -o ServerAliveInterval=15 -o ServerAliveCountMax=3 "$USER@$SERVER"
fi
