# 远端链接教程（全自动，无需管理员、无需拷贝脚本）

> **线上部署说明（必看）**：`us.cxu.lol` 主站是 SPA，不要动它。
> frp 走你新增的 **`proxy.cxu.lol`** 独立部署（`frp/deploy/` 一键脚本），下面所有命令都用 `proxy.cxu.lol`。
> A 类静态根只保留给**本机开发**用（`--static-root`），线上 `deploy-proxy.sh` 默认关闭（`/` 只做 get 落地）。

远端只做一件事，粘贴这一行（把 `colab1` 换成你的名）：

```bash
curl -fsSL http://proxy.cxu.lol/get.sh | NAME=colab1 bash
```

做完打开 `https://proxy.cxu.lol/colab1/` 即达远端 `127.0.0.1:43110`。
浏览器打开 `http://proxy.cxu.lol/get?html` 也有带输入框的落地页，可复制命令。

## 0. 线上部署（在 31.58.87.230 root 下执行一次）

```bash
# 1. DNS: proxy.cxu.lol -> 31.58.87.230（先加解析，等生效）
# 2. 拷 deploy/ 上去并执行
scp -r frp/deploy root@31.58.87.230:/root/frp-deploy
ssh root@31.58.87.230 'cd /root/frp-deploy && chmod +x *.sh && ./deploy-proxy.sh'
# 脚本做: 装bun+nginx → 建tunnel用户+sshd-tunnel → 起systemd frp(:8080) → 配nginx代理 → 自检
# 3. 证书（proxy 独立证书，不碰 us 主站）：
certbot --nginx -d proxy.cxu.lol   # 或按现有 acme 流程签发后填进 nginx-proxy.cxu.lol.conf
# 4. 验证（部署完这几条必须通，之前远端失败就是因为后端没上）：
curl -s http://proxy.cxu.lol/get.sh | head -3        # 必须是 #!/usr/bin/env bash，不是 HTML
curl -s http://proxy.cxu.lol/__health                # {"ok":true,...}
curl -s http://proxy.cxu.lol/__enroll/help           # 口令算法
# 然后再让远端跑一键命令。原生 ssh -R 不经 enroll 也行（见 §5 备选）。
```

目标：远端主机 `a` 上跑着 `127.0.0.1:43110`（如 Colab/gradio/anything），
注册名 `colab1` 后，浏览器打开 `https://proxy.cxu.lol/colab1/` 即可访问。

## 0. 先看懂分级（30 秒）

- **A 类（静态/永久）**：`/` 根路径固定反代**本机 `127.0.0.1:43110`**，常驻、不走 24h 过期。
  远端动态隧道走 `/<name>/`（B 类，24h 有效），互不冲突。`get/join` 等为系统保留名，不可注册。
- **B 类（动态/24h）**：链路如下

```
浏览器 --HTTPS--> proxy.cxu.lol/colab1/ --通配反代--> 本机 127.0.0.1:8080 (frp server, tmux frp)
  --按首段 /colab1 路由--> 本机 127.0.0.1:14000 (sshd 监听)
  --ssh -R 反向隧道--> 远端 127.0.0.1:43110
```

Nginx/Caddy 只配一次通配反代，之后加名字/过期都不用动它（见 `deploy/`）。

## 1. 一键接入（推荐，1 条命令，无需拷贝脚本）

远端要求：Linux + `openssh-client openssl curl`（Colab/Ubuntu 默认全有），**出站能连 `proxy.cxu.lol:443` 和 `:22` 即可**，无需公网 IP、无需开防火墙入站。

```bash
curl -fsSL http://proxy.cxu.lol/get.sh | NAME=colab1 bash
```

脚本自动做 4 件事（全程无人工）：

| 步骤 | 做了什么 | 对应命令（脚本已包好，手动版备用） |
|---|---|---|
| 1/4 | 按时间规则算动态口令 | `CODE=$(echo -n "cxu.lol:$(( $(date +%s) / 300 ))" \| sha256sum \| cut -c1-8)` —— 即 `sha256("cxu.lol:<step>")[0:8]`，`step=floor(now/300)`，服务端容忍 ±1 步 |
| 2/4 | 生成 ssh key（已有则跳过） | `ssh-keygen -t ed25519 -f ~/.ssh/tunnel -N "" -C "colab1"` |
| 3/4 | 加密上报 pubkey，服务端自动绑定+分配端口 | `ENC=$(openssl enc -aes-256-cbc -pbkdf2 -pass pass:$CODE -in ~/.ssh/tunnel.pub \| base64 \| tr -d '\n')` 然后 `curl -XPOST https://proxy.cxu.lol/__enroll -d '{"name":"colab1","code":"$CODE","pubkey_enc":"..."}'` → 返回 `{"port":14000,"publicUrl":"https://proxy.cxu.lol/colab1/","sshCmd":"..."}` |
| 4/4 | 建隧道并保活 | `ssh -NTR 127.0.0.1:14000:127.0.0.1:43110 -i ~/.ssh/tunnel tunnel@proxy.cxu.lol`（有 autossh 则自动用它） |

成功后浏览器打开 **`https://proxy.cxu.lol/colab1/`**，即达远端 `43110`。

自定义变量：`NAME=xx SERVER=proxy.cxu.lol USER=tunnel LOCAL=43110 KEY=~/.ssh/tunnel ./remote-auto.sh`
（`LOCAL` 是远端被代理的端口，默认 43110；`PORT` 不用传，服务端 enroll 自动分配。）

## 2. 手动分步版（脚本跑不通时用）

```bash
# --- 远端 ---
NAME=colab1
[ -f ~/.ssh/tunnel ] || ssh-keygen -t ed25519 -f ~/.ssh/tunnel -N "" -C "$NAME" -q
CODE=$(echo -n "cxu.lol:$(( $(date +%s) / 300 ))" | sha256sum | cut -c1-8)
ENC=$(openssl enc -aes-256-cbc -pbkdf2 -pass pass:$CODE -in ~/.ssh/tunnel.pub | base64 | tr -d '\n')
curl -sS -XPOST https://proxy.cxu.lol/__enroll -H 'content-type: application/json' \
  -d "{\"name\":\"$NAME\",\"code\":\"$CODE\",\"pubkey_enc\":\"$ENC\"}"
# => {"ok":true,"name":"colab1","port":14000,"publicUrl":"https://proxy.cxu.lol/colab1/",
#     "sshCmd":"ssh -NTR 127.0.0.1:14000:127.0.0.1:43110 -i ~/.ssh/tunnel tunnel@proxy.cxu.lol"}
# 记下 port，把 sshCmd 粘过去执行（前台；加 -fN 可后台，autossh 可保活）：
ssh -NTR 127.0.0.1:14000:127.0.0.1:43110 -i ~/.ssh/tunnel tunnel@proxy.cxu.lol
```

## 3. 备选：WS 隧道（远端有 Bun、但 22 端口出站被封时用）

```bash
# 远端（只需出站 443，无需 ssh）
bun src/client.ts --server wss://proxy.cxu.lol/__tunnel --name colab1 --upstream http://127.0.0.1:43110
# 打开 https://proxy.cxu.lol/colab1/
```

## 4. 排错对照表

| `get.sh` 拉下来是 HTML（`!doctype` 报错） | 命中了主站 SPA，后端没部署/域名指错 | 先按 §0 在 `proxy.cxu.lol` 部署；验证 `curl -s http://proxy.cxu.lol/get.sh \| head -3` 必须是 `#!/usr/bin/env bash` |
| 打开页面是 SPA / `POST /__enroll` 返回 `{"error":"Not found"}` | 请求根本没到 frp，被主站 nginx 吃了 | 同上：frp 必须走 `proxy.cxu.lol` 独立 server 块，不要挂到 `us.cxu.lol` 下 |
|---|---|---|
| `enroll` 返回 403 `bad code` | 远端/服务端时钟差 >5 分钟 | 远端 `date -u` 对时；5 分钟窗口内重跑（code 每 300s 一变，旧 code 立刻失效） |
| `enroll` 返回 400 `decrypt pubkey fail` | openssl 版本/参数不对 | 用脚本原命令（含 `-pbkdf2`）；不要改加密参数；pubkey 必须是单行 `ssh-ed25519 AAAA...` |
| `enroll` 返回 409 `name taken/reserved` | 名字被占或为保留名 | 换 `NAME`（规则 `[a-z0-9-]`，32 字符内；`get/join` 等保留名不可用） |
| 打开页面 502 `ssh tunnel offline` | `ssh -R` 没连上/断了 | 回远端看隧道进程是否还在；重跑脚本；服务端 `curl localhost:8080/__health` 看 ssh 列表 |
| 打开页面 404 `no such tunnel` | 名字没注册或已过期 | 重跑 enroll；24h 到期后重调一次即续期 |
| 打开页面 410 `expired` | 24h 到期 | 重调 `POST /__enroll`（code 重算）续期，再重连 `ssh -R` |
| `ssh tunnel@...` 直接 `Permission denied` | key 还没 enroll 绑定 | 先跑 enroll（绑定 authorized_keys），再 `ssh -NTR`；直连 ssh 永远拿不到 shell（`ForceCommand /bin/false`，正常） |

## 5. 不走 enroll：原生 ssh -R（你有线上 ssh 账号时用）

```bash
# 线上（root，一次）：给远端开一个只能 -R 的账号（复用 tunnel 用户即可），手动绑公钥
# 远端把 ~/.ssh/tunnel.pub 发你，你执行：
cat id-remote.pub >> /home/tunnel/.ssh/authorized_keys
# 远端直接映射（端口你口头指定，如 14001，不经 enroll 自动分配）：
ssh -NTR 127.0.0.1:14001:127.0.0.1:43110 -i ~/.ssh/tunnel tunnel@proxy.cxu.lol
# 但注意：这条通道 frp server 不知道，必须再手动登记路由，否则 /colab1/ 还是 404。
# 登记（线上执行，ADMIN_TOKEN 鉴权）：
curl -sXPOST -H 'x-admin-token: secret' localhost:8080/__admin/alloc \
  -H 'content-type: application/json' -d '{"name":"colab1"}'
# 返回的 port 必须和上面 ssh -R 的端口一致（14001），否则访问 502。
# 所以推荐还是走 enroll（一键命令自动保证两边端口一致），原生只应急用。
```

## 6. 服务端速查（管理员）

```bash
tmux attach -t frp            # 看日志；Ctrl-b d 脱离
curl -s localhost:8080/__health   # ws/ssh 在线表
curl -s localhost:8080/__routes   # 含剩余有效期
curl -s localhost:8080/__enroll/help  # 公开的口令算法说明
tmux kill-session -t frp && bun frp/start-tmux.ts  # 重启
```

启动参数：`--enroll-secret cxu.lol --enroll-step-sec 300 --auth-keys /home/tunnel/.ssh/authorized_keys --ttl-hours 24`。
sshd 一次性配置见 `deploy/sshd-tunnel.conf` + `deploy/setup-ssh.sh`。
