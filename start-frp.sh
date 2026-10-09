#!/usr/bin/env bash
# ==============================================================================
# start-frp.sh - frp-mini 服务端一键启动 / 管理脚本 (适用于 Serv00 / Linux)
# ==============================================================================
set -euo pipefail

# 基础路径配置
FRP_DIR="${FRP_DIR:-$HOME/frp-mini}"
PORT="${PORT:-12913}"
PUBLIC_HOST="${PUBLIC_HOST:-dev.cxu.lol}"
TMUX_SESSION="${TMUX_SESSION:-cxu}"
TMUX_WINDOW="${TMUX_WINDOW:-frp}"

# 寻找 Bun 运行时
if [ -x "$HOME/bin/bun" ]; then
  BUN="$HOME/bin/bun"
elif [ -x "$HOME/.bun/bin/bun" ]; then
  BUN="$HOME/.bun/bin/bun"
elif command -v bun >/dev/null 2>&1; then
  BUN="$(command -v bun)"
else
  echo "❌ 未找到 bun 运行时，请检查 ~/bin/bun 或 ~/.bun/bin/bun"
  exit 1
fi

get_pid() {
  pgrep -f "bun.*src/server\.ts.*${PORT}" 2>/dev/null || true
}

cmd_start() {
  local pid
  pid=$(get_pid)
  if [ -n "$pid" ]; then
    echo "⚠️ frp 服务已在运行中 (PID: $pid, 端口: $PORT)"
    echo "👉 访问地址: https://${PUBLIC_HOST}/"
    return 0
  fi

  echo "🚀 正在启动 frp-mini 服务端 (端口: ${PORT}, 域名: ${PUBLIC_HOST})..."

  if command -v tmux >/dev/null 2>&1; then
    # 优先在 tmux 中运行，防止 SSH 断开影响
    if tmux has-session -t "$TMUX_SESSION" 2>/dev/null; then
      # 杀掉可能残留的旧 window
      tmux kill-window -t "${TMUX_SESSION}:${TMUX_WINDOW}" 2>/dev/null || true
      tmux new-window -t "$TMUX_SESSION" -n "$TMUX_WINDOW" -c "$FRP_DIR" \
        "$BUN src/server.ts --http $PORT --public-host $PUBLIC_HOST"
    else
      tmux new-session -d -s "$TMUX_SESSION" -n "$TMUX_WINDOW" -c "$FRP_DIR" \
        "$BUN src/server.ts --http $PORT --public-host $PUBLIC_HOST"
    fi
  else
    # 无 tmux 环境回退为 nohup 后台
    nohup "$BUN" "$FRP_DIR/src/server.ts" --http "$PORT" --public-host "$PUBLIC_HOST" \
      > "$FRP_DIR/server.log" 2>&1 &
  fi

  sleep 2
  pid=$(get_pid)
  if [ -n "$pid" ]; then
    echo "✅ frp-mini 已成功启动！"
    echo "   - 进程 PID : $pid"
    echo "   - 监听端口 : $PORT"
    echo "   - 对外域名 : https://${PUBLIC_HOST}/"
    echo "   - 健康检查 : curl -s http://127.0.0.1:${PORT}/__health"
  else
    echo "❌ 启动失败，请检查运行环境或端口占用情况"
    exit 1
  fi
}

cmd_stop() {
  local pid
  pid=$(get_pid)
  if [ -z "$pid" ]; then
    echo "ℹ️ frp 服务未在运行"
    return 0
  fi

  echo "🛑 正在停止 frp 服务 (PID: $pid)..."
  kill "$pid" 2>/dev/null || true
  sleep 1

  if command -v tmux >/dev/null 2>&1 && tmux has-session -t "$TMUX_SESSION" 2>/dev/null; then
    tmux kill-window -t "${TMUX_SESSION}:${TMUX_WINDOW}" 2>/dev/null || true
  fi

  pid=$(get_pid)
  if [ -z "$pid" ]; then
    echo "✅ frp 服务已停止"
  else
    kill -9 "$pid" 2>/dev/null || true
    echo "✅ frp 服务已强制终止"
  fi
}

cmd_status() {
  local pid
  pid=$(get_pid)
  if [ -n "$pid" ]; then
    echo "🟢 frp 服务运行中 (PID: $pid, 端口: $PORT)"
    echo "   - 对外域名 : https://${PUBLIC_HOST}/"
    echo "   - 进程资源 :"
    ps -p "$pid" -o pid,stat,time,%mem,command 2>/dev/null || ps -p "$pid" 2>/dev/null || true
    echo ""
    echo "   - 活跃隧道路由 :"
    curl -s -m 3 "http://127.0.0.1:${PORT}/__routes" 2>/dev/null || echo "(暂时无法读取路由)"
    echo ""
  else
    echo "🔴 frp 服务未运行"
  fi
}

cmd_logs() {
  if command -v tmux >/dev/null 2>&1 && tmux has-session -t "${TMUX_SESSION}:${TMUX_WINDOW}" 2>/dev/null; then
    tmux capture-pane -pt "${TMUX_SESSION}:${TMUX_WINDOW}" -S -50
  elif [ -f "$FRP_DIR/server.log" ]; then
    tail -n 50 "$FRP_DIR/server.log"
  else
    echo "ℹ️ 暂无日志输出"
  fi
}

ACTION="${1:-start}"
case "$ACTION" in
  start)
    cmd_start
    ;;
  stop)
    cmd_stop
    ;;
  restart)
    cmd_stop
    sleep 1
    cmd_start
    ;;
  status)
    cmd_status
    ;;
  logs|log)
    cmd_logs
    ;;
  *)
    echo "用法: $0 {start|stop|restart|status|logs}"
    exit 1
    ;;
esac
