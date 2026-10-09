# frp-lite：`https://us.cxu.lol/<name>` → 远端 `127.0.0.1:43110`

> 远端链接看 **`TUTORIAL.md`**（一键：`NAME=colab1 ./remote-auto.sh`）。
> 服务端已在 tmux `frp` 会话后台运行（`:8080`）。

结论先行（能做，用什么语言）：
- **能做，不用动态改 Nginx/Caddy。** 入口只配一次通配反代 ` us.cxu.lol/* → 127.0.0.1:8080`，
  由本服务按 URL 首段 `/:name/*` 路由到对应远端，24h 到期内存+落盘自动失效。
- **语言：Bun + TypeScript 单进程**（`src/server.ts`），零依赖，`fetch` 原生做反代。
- **远端免客户端（推荐）：`ssh -R`，远端只有 OpenSSH 就行**（Colab/任何 Linux 默认都有），
  不用装 frpc/cloudflared。备选才用 `src/client.ts`（类 cloudflared 的 WS 隧道，断线自重连）。

## 两种远端接入

| 方式 | 远端要装什么 | 注册 | 访问 |
|---|---|---|---|
| A. `ssh -R`（最简，推荐） | 仅 openssh 客户端 | 先调 `POST /__admin/alloc` 拿端口，再 `ssh -NTR` | `https://us.cxu.lol/colab1/` → 本机 `127.0.0.1:<port>` →（sshd 转发）→ 远端 `127.0.0.1:43110` |
| B. WS隧道 | `bun src/client.ts` | 出站 WS 连 `/__tunnel` 注册 `name` | `https://us.cxu.lol/colab1/` → WS 透传到远端 `http://127.0.0.1:43110` |

A 本质就是“端口反代”：sshd 把远端 43110 映射到本机 `127.0.0.1:14000+`，
server 再把 `/:name/*` 反代到那个本地端口。Nginx/Caddy 完全无感，不用 reload。

## 快速开始（服务端）

```bash
cd frp
TUNNEL_TOKEN=xxx ADMIN_TOKEN=yyy SSH_HOST=us.cxu.lol SSH_USER=tunnel \
  bun src/server.ts --http 8080 --ttl-hours 24 --ssh-base-port 14000
# 健康/路由表
curl -s localhost:8080/__health; curl -s localhost:8080/__routes
```

入口（只需配一次，任选其一，详见 `deploy/`）：
- Caddy：`deploy/Caddyfile` —— 整站 `reverse_proxy 127.0.0.1:8080`，TLS 自动。
- Nginx：`deploy/nginx-us.cxu.lol.conf` —— `location / { proxy_pass http://127.0.0.1:8080; ... }` 兼容 WS Upgrade。

## 远端 A：ssh -R（零安装）

```bash
# 1. 远端生成 key（一次即可）
ssh-keygen -t ed25519 -f ~/.ssh/tunnel -N "" -C "colab1"
cat ~/.ssh/tunnel.pub   # 把公钥发给管理员，追加到服务端 tunnel 用户 authorized_keys

# 2. 本机/管理员分配名字+端口（24h 有效，可续期；ADMIN_TOKEN 鉴权）
curl -sXPOST -H 'x-admin-token: yyy' localhost:8080/__admin/alloc \
  -H 'content-type: application/json' -d '{"name":"colab1"}'
# => {"ok":true,"name":"colab1","port":14000,"publicUrl":"https://us.cxu.lol/colab1/",
#     "sshCmd":"ssh -NTR 127.0.0.1:14000:127.0.0.1:43110 tunnel@us.cxu.lol"}

# 3. 远端执行（把 远端43110 暴露到 服务端127.0.0.1:14000；autossh 保活）
ssh -NTR 127.0.0.1:14000:127.0.0.1:43110 -i ~/.ssh/tunnel tunnel@us.cxu.lol
# 浏览器打开 https://us.cxu.lol/colab1/ 即达远端 43110
```

服务端 sshd 侧（一次性，见 `deploy/sshd-tunnel.conf` + `deploy/setup-ssh.sh`）：
`AllowTcpForwarding remote`, `GatewayPorts no`（只绑 127.0.0.1，最小暴露面），
`tunnel` 用户 `ForceCommand /bin/false` + `PermitTTY no`，只能做 `-R` 转发不能拿 shell。

## 远端 B：WS 隧道（类 cloudflared，有 Bun 时用）

```bash
# 远端（需出站 443/80 即可，无需公网 IP / 无需开端口）
bun src/client.ts --server wss://us.cxu.lol/__tunnel --name colab1 --upstream http://127.0.0.1:43110
# 打开 https://us.cxu.lol/colab1/
```

## 24h 有效怎么实现

- 注册/分配时写 `expiresAt = now + TTL`（默认 24h，`--ttl-hours` 可调）。
- WS：到期 `ws.close()` 踢下线，`/:name` 立刻 404/502；客户端重连会重新注册=重新计时。
- ssh：到期删 `routes.json` 映射、端口回收；已建的 `-R` 通道不断但访问回 410 `expired, re-alloc`，
  远端重调一次 `/__admin/alloc`（续期）即可。`setInterval(sweep, 60s)` + 落盘 `data/routes.json`，重启不丢。
- Nginx/Caddy 侧**零操作**：它们只认通配 `/*`，增删路由不 reload、不 `nginx -s reload`。

## 为什么不用逐路由写 Nginx

每加一个名字就改 conf + reload：reload 有抖动、并发写易坏、还要处理证书/锁。
路径首段路由把“控制面”（注册/过期）收敛在一个进程里，入口层保持静态，
这正是 cloudflared/serveo 的做法。真要按子域 `colab1.us.cxu.lol` 才需要动态 DNS+泛证书，
按路径 `us.cxu.lol/colab1` 则完全不需要。
