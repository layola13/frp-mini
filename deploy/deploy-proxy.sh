#!/usr/bin/env bash
# 线上服务端一键部署(proxy.cxu.lol 独立,不碰 us.cxu.lol 主站)
# 在 31.58.87.230 上以 root 执行: bash deploy-proxy.sh
# 前提: DNS 已加 proxy.cxu.lol -> 31.58.87.230; 本脚本装 bun/nginx/sshd-tunnel/frp/systemd
set -euo pipefail
APP_DIR="/opt/frp"
PORT="8080"
TTL_HOURS="24"
STATIC_ROOT="${STATIC_ROOT:-}"   # 线上建议留空: / 根只做 get 落地+404,不反代本地43110,避免与主站混淆

command -v bun >/dev/null || { curl -fsSL https://bun.sh/install | bash; export PATH="$HOME/.bun/bin:$PATH"; }
command -v nginx >/dev/null || { apt-get update -y && apt-get install -y nginx openssh-server; }
id tunnel &>/dev/null || useradd -m -s /bin/false tunnel
install -d -m 700 -o tunnel -g tunnel /home/tunnel/.ssh
touch /home/tunnel/.ssh/authorized_keys
chown tunnel:tunnel /home/tunnel/.ssh/authorized_keys
chmod 600 /home/tunnel/.ssh/authorized_keys
cp sshd-tunnel.conf /etc/ssh/sshd_config.d/tunnel.conf
sshd -t && systemctl reload sshd

mkdir -p "$APP_DIR"
cp -r src package.json remote-auto.sh "$APP_DIR"/
mkdir -p "$APP_DIR/data"

cat > /etc/systemd/system/frp.service <<EOF
[Unit]
Description=frp-lite tunnel server (proxy.cxu.lol)
After=network.target sshd.service
[Service]
Type=simple
WorkingDirectory=$APP_DIR
Environment=PUBLIC_HOST=proxy.cxu.lol
Environment=SSH_HOST=proxy.cxu.lol
Environment=SSH_USER=tunnel
Environment=STATIC_ROOT=$STATIC_ROOT
Environment=ADMIN_TOKEN=${ADMIN_TOKEN:-secret}
Environment=TUNNEL_TOKEN=${TUNNEL_TOKEN:-}
ExecStart=/root/.bun/bin/bun src/server.ts --http $PORT --ttl-hours $TTL_HOURS --db ./data/routes.json
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now frp.service

cp nginx-proxy.cxu.lol.conf /etc/nginx/conf.d/proxy.cxu.lol.conf
nginx -t && systemctl reload nginx

echo "---- verify ----"
sleep 2
systemctl is-active frp.service
curl -sS localhost:$PORT/__health
echo
echo "远端一键: curl -fsSL http://proxy.cxu.lol/get.sh | NAME=colab1 bash"
echo "浏览器:   https://proxy.cxu.lol/colab1/"
