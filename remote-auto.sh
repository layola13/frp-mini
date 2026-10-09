#!/usr/bin/env bash
# 远端全自动接入: https://proxy.cxu.lol/<name> -> 本机 127.0.0.1:43110
# 全程无需管理员: 自动生成唯一主机名, 自动生成密钥, 加密上报, 服务端分配端口并建立隧道
# 用法: curl -fsSL http://proxy.cxu.lol/get.sh | bash
# 也可自定义名称: curl -fsSL http://proxy.cxu.lol/get.sh | NAME=mycustom bash
set -euo pipefail

SERVER="${SERVER:-proxy.cxu.lol}"
USER="${USER:-tunnel}"
LOCAL="${LOCAL:-43110}"
KEY="${KEY:-$HOME/.ssh/tunnel}"
SECRET="${SECRET:-cxu.lol}"
STEP="${STEP:-300}"
SCHEME="${SCHEME:-https}"
FG="${FG:-0}"

echo "=================================================="
echo "🚀 正在建立 Codex Ultra 远端反代接入..."
echo "=================================================="

# 1. 确保 SSH 密钥存在（如不存在则自动生成）
mkdir -p "$(dirname "$KEY")"
chmod 700 "$(dirname "$KEY")" 2>/dev/null || true
if [ ! -f "$KEY" ]; then
  echo "[1/4] 生成独立隧道密钥: $KEY"
  ssh-keygen -t ed25519 -f "$KEY" -N "" -q
else
  echo "[1/4] 复用已有隧道密钥: $KEY"
fi

# 2. 自动生成不重复的机器名称（基于 hostname + 密钥指纹，同机重连名称永久固定）
if [ -z "${NAME:-}" ]; then
  RAW_HOST=$(hostname 2>/dev/null || cat /etc/hostname 2>/dev/null || echo "node")
  CLEAN_HOST=$(echo "$RAW_HOST" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9' | cut -c1-12)
  CLEAN_HOST="${CLEAN_HOST:-node}"
  FP=$(sha256sum "$KEY.pub" 2>/dev/null | cut -c1-6 || openssl rand -hex 3 2>/dev/null || echo "$$")
  NAME="${CLEAN_HOST}-${FP}"
fi

# 3. 动态口令计算与公钥加密上报
NOW=$(date +%s)
STEP_N=$(( NOW / STEP ))
CODE=$(echo -n "${SECRET}:${STEP_N}" | sha256sum | cut -c1-8)
echo "[2/4] 请求注册名: $NAME (验证码已就绪)"

echo "[3/4] 加密公钥并向服务端注册分配端口..."
ENC=$(openssl enc -aes-256-cbc -pbkdf2 -pass "pass:$CODE" -in "$KEY.pub" | base64 | tr -d '\n')
PAYLOAD=$(python3 -c "import json,sys;print(json.dumps({'name':sys.argv[1],'code':sys.argv[2],'pubkey_enc':sys.argv[3]}))" "$NAME" "$CODE" "$ENC" 2>/dev/null || \
  node -e "console.log(JSON.stringify({name:process.argv[1],code:process.argv[2],pubkey_enc:process.argv[3]}))" "$NAME" "$CODE" "$ENC")
RESP=$(curl -sS --max-time 20 -XPOST "$SCHEME://$SERVER/__enroll" -H 'content-type: application/json' -d "$PAYLOAD")

PORT=$(echo "$RESP" | grep -o '"port":[0-9]*' | head -1 | cut -d: -f2 || true)
if [ -z "${PORT:-}" ]; then
  echo "❌ 注册失败: $RESP"
  echo "提示: 若报 bad code 请检查远端时钟与网络时间是否同步 (偏差需 < 5分钟)"
  exit 1
fi

ASSIGNED_NAME=$(echo "$RESP" | grep -o '"name":"[^"]*"' | head -1 | cut -d'"' -f4 || true)
ASSIGNED_NAME="${ASSIGNED_NAME:-$NAME}"
PUBLIC_URL=$(echo "$RESP" | grep -o '"publicUrl":"[^"]*"' | head -1 | cut -d'"' -f4 || true)
PUBLIC_URL="${PUBLIC_URL:-$SCHEME://$SERVER/$ASSIGNED_NAME/}"

# 4. 建立反向隧道（默认后台守护运行，支持自动重连；关闭终端不影响隧道）
pkill -f "ssh.*$KEY.*$SERVER" 2>/dev/null || true

if [ "$FG" = "1" ]; then
  echo "[4/4] 正在前台建立隧道 (映射端口: $PORT)..."
  echo ""
  echo "=================================================="
  echo "✅ 隧道已成功建立！"
  echo "👉 请在浏览器中打开: $PUBLIC_URL"
  echo "=================================================="
  echo ""
  exec ssh -NTR "127.0.0.1:$PORT:127.0.0.1:$LOCAL" -i "$KEY" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ServerAliveInterval=15 -o ServerAliveCountMax=3 "$USER@$SERVER"
else
  echo "[4/4] 正在后台启动隧道守护进程 (映射端口: $PORT)..."
  nohup bash -c "
    while true; do
      ssh -NTR \"127.0.0.1:$PORT:127.0.0.1:$LOCAL\" -i \"$KEY\" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ServerAliveInterval=15 -o ServerAliveCountMax=3 \
        -o ExitOnForwardFailure=yes \"$USER@$SERVER\" 2>/dev/null || true
      sleep 3
    done
  " >/dev/null 2>&1 &
  TUNNEL_PID=$!
  sleep 1.5

  echo ""
  echo "=================================================="
  echo "✅ 隧道已在后台成功启动！(PID: $TUNNEL_PID)"
  echo "👉 请在浏览器中打开: $PUBLIC_URL"
  echo "💡 提示: 隧道已后台守护运行，即便关闭当前终端窗口也依然有效！"
  echo "   (如需停止隧道，可执行: kill $TUNNEL_PID)"
  echo "=================================================="
  echo ""
fi
