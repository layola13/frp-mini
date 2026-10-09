#!/usr/bin/env bash
# 服务端一次性初始化：建 tunnel 用户 + sshd 配置 + 目录（需 root）
set -euo pipefail
id tunnel &>/dev/null || useradd -m -s /bin/false tunnel
install -d -m 700 -o tunnel -g tunnel /home/tunnel/.ssh
touch /home/tunnel/.ssh/authorized_keys
chown tunnel:tunnel /home/tunnel/.ssh/authorized_keys
chmod 600 /home/tunnel/.ssh/authorized_keys
cp sshd-tunnel.conf /etc/ssh/sshd_config.d/tunnel.conf
sshd -t && systemctl reload sshd
echo "ok. 追加远端公钥: cat id.pub >> /home/tunnel/.ssh/authorized_keys"
echo "远端接入: ssh -NTR 127.0.0.1:<port>:127.0.0.1:43110 -i ~/.ssh/tunnel tunnel@us.cxu.lol"
