# frp-go（Go 分支）

TypeScript 版（`main`/`ws` 分支，Bun）的 Go 重写：单二进制、`--mode` 切换形态，
无运行时依赖（`vendor/` 已提交，`GOPROXY=off` 也能编），内存/CPU 更稳。

## 为什么重写：两个生产问题

| 问题 | TS 现状 | Go 版 |
|---|---|---|
| 长 turn 必死 | 服务端 `UPSTREAM_TIMEOUT_MS = 25_000` 硬编码；客户端 `await r.arrayBuffer()` 整包缓冲。SSE 永远播不出来，>25s 的 turn 一律 504 | `--upstream-timeout` 可配（默认 5m）+ `res_start/res_chunk/res_end` 流式分片，首字节即时到达 |
| 一机一端口 | 一个 agent 名只能 `--upstream` 一个本地端口 | `--upstream /a=http://127.0.0.1:43210,/b=http://127.0.0.1:18080` 按路径前缀路由 + `--name a,b` 单连接多注册 |

## 用法

```bash
go build -o frp-go ./cmd/frp-go   # 或：GOPROXY=off go build -mod=vendor ./...

# 服务端（WS 模式，go分支默认端口与main错开：http 18080 / ssh基址 15000 / 回源 43210）
./frp-go --mode ws-server --http 18080 --ttl-hours 24 --upstream-timeout 5m \
  --public-host dev.cxu.lol --db ./data/routes.json

# 客户端：一个隧道带多个本地端口
./frp-go --mode ws-client --server wss://dev.cxu.lol/__tunnel \
  --name codex,api --upstream '/codex=http://127.0.0.1:43210,/api=http://127.0.0.1:18080'

# 客户端：ssh -R 反向（免 openssh，自动 enroll）
./frp-go --mode ssh-client --server dev.cxu.lol --name colab1 --local 43210
```

## 协议兼容

- 与 TS 新旧两端互通：Go 服务端同时接受整包 `res` 与分片 `res_*`；
  Go 客户端看 `registered.streaming` 决定分片还是整包（旧服务端无该字段 → 自动降级整包）。
- 服务端读同一 `routes.json`（ssh `-R` 路由与 TS 服务端共享，迁移期混跑无碍）。
- `resident` 保留名、TTL（24h）、`__health`/`__routes`、HTML 改写、cookie/referer 回退与 TS一致。

## 本分支未做（phase 2）

- 落地页 HTML 版（`/get` 只有文本版）；`ssh-client --local` 暂只支持单端口。
- 生产部署切换（建议先在 dev 机旁跑 Go `ws-server` 读同一 `routes.json` 混跑）。
- 已验证：原版 openssh `get.sh` 全流程（enroll→ssh-R→回源）本地实跑通过；
  `/get.sh`（ws 版）与 `/ssh.sh`（原版）双版本同服。
