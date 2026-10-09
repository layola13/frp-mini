# Serv00 部署与 WebSocket 纯免 SSH 隧道指南 (`ws` 分支)

本文档记录在资源受限环境（如 **Serv00** 虚拟主机）下部署 `frp-mini` 的全套架构设计、操作步骤及排障经验。

---

## 1. 架构背景与设计选型

### Serv00 的环境限制
1. **严格的进程数上限**：Serv00 限制单个账户最大进程数不超过 20 (`max user processes = 20`)。
2. **严格的端口限制**：每个账户仅有 3 个预留端口配额，无法像独立 Linux 主机一样动态开放 `14000+` 端口。
3. **无 Root 权限**：无法修改系统 `sshd_config`，无法新建独立的 `tunnel` 隔离用户，无法配置强制命令和网关端口。

### 纯 WebSocket 隧道方案优势 (`ws` 分支)
- **单进程架构**：服务端以 FreeBSD 原生 Bun 单进程运行（进程数占用：**仅 1 个**），内存占用极低。
- **零额外端口**：所有控制信令、HTTP 数据反代、全双工 WebSocket 均复用同一个对外 HTTP(S) 端口（如 `12913`）。
- **远端免配置**：远端（Colab、AutoDL、本地开发机等）无需 OpenSSH 客户端或公网 IP，只需出站连接 `wss://`，断线自动重连。
- **全双工 WS 透传**：完美支持前端 Codex Ultra 的 `/ws` 协议（心跳握手、RPC 与实时事件流）。

---

## 2. 服务端部署（Serv00）

### 2.1 启动服务
在 Serv00 的 tmux 会话中启动服务端：
```bash
cd ~/frp-mini
# 以保留端口 12913 为例，对外域名 dev.cxu.lol
/home/$(whoami)/bin/bun src/server.ts --http 12913 --public-host dev.cxu.lol
```

### 2.2 域名与反代配置
使用 Serv00 自带的 `devil` CLI 工具配置域名：
```bash
# 1. 添加反向代理域名到本地服务端口
devil www add dev.cxu.lol proxy localhost 12913
```

---

## 3. 核心暗坑排查与关键配置（必读）

### ⚠️ 暗坑一：SSL 证书缺失引发“域名错位路由”与 403 Forbidden
- **故障现象**：
  - 浏览器访问页面提示 `Not Secure`（不安全）。
  - 网页主界面可以加载，但点击保存设置（如 SA-VM 配置）时报错：
    `Failed to load resource: the server responded with a status of 403 (Forbidden)`
    `failed to sync workspace skill on save Error: Failed to sync SA-VM workspace skill (403)`
- **根本原因**：
  - Serv00 前端 Nginx 使用 SNI 路由。
  - 若只在 `devil www` 添加了代理，但未在 `devil ssl www` 为该域名申请 SSL 证书，当浏览器通过 HTTPS 访问时，Nginx 会回退匹配同 IP 下的默认证书域名（如 `cxu.lol`）。
  - 导致 HTTPS 请求被错误转发到了 `cxu.lol` 对应的后端——即 Serv00 本地常驻的 `43110` 实例，而非 Colab 隧道！本地实例校验 `/content` 工作区路径与 Token 失败，直接拦截并返回 403。
- **修复命令**：
  ```bash
  # 为 dev.cxu.lol 申请并绑定免费 Let's Encrypt 证书（IP 请按实际填写，通常为 188.68.250.201）
  devil ssl www add 188.68.250.201 le le dev.cxu.lol
  ```

### ⚠️ 暗坑二：Serv00 默认 WAF 误杀 POST 策略数据
- **故障现象**：
  - 保存复杂策略、脚本权限等 JSON 配置时被莫名拦截报 403。
- **根本原因**：
  - Serv00 为新加域名默认启用了 `Web Application Firewall: Level 1` 和 `Blacklist: Level 1`。
  - 请求体中包含的 shell 命令、执行规则字段被 WAF 判定为潜在脚本注入。
- **修复命令**：
  ```bash
  # 关闭该域名的 WAF 与黑名单过滤
  devil www options dev.cxu.lol waf 0
  devil www options dev.cxu.lol blacklist 0
  ```

### ⚠️ 暗坑三：开启强制 SSL
- **修复命令**：
  ```bash
  # 开启全站 301 强制跳转 HTTPS，消除浏览器 "Not secure" 警告
  devil www options dev.cxu.lol sslonly on
  ```

---

## 4. 远端一键接入（Colab / Linux）

在远端运行 Codex Ultra（本地 `127.0.0.1:43110`）的主机上执行：

```bash
# 全自动接入（自动检测/安装轻量 Bun，生成唯一标识并后台守护启动）
curl -fsSL https://dev.cxu.lol/get.sh | bash
```

或自定义固定名称：
```bash
curl -fsSL https://dev.cxu.lol/get.sh | NAME=mycolab bash
```

### 运行机制
1. 脚本自动检测当前环境，如果未安装 Bun，则自动秒级静默拉取 Bun 运行时。
2. 自动拉取客户端驱动 `client.ts`。
3. 后台守护建立双向 WebSocket 隧道连接 `wss://dev.cxu.lol/__tunnel`。
4. 控制台输出专属 HTTPS 访问链接：`https://dev.cxu.lol/<name>/`。

---

## 5. 常用运维与检查指令

| 检查项 | 命令 |
|---|---|
| 查看域名及 WAF/SSL 配置 | `devil www list -v` |
| 查看 SSL 证书到期状态 | `devil ssl www list` |
| 检查当前账户运行进程数 | `ps -U $(whoami)` |
| 查看 frp-mini 隧道活跃路由 | `curl -s https://dev.cxu.lol/__routes` |
| 检查服务端健康状态 | `curl -s https://dev.cxu.lol/__health` |
| 查看 tmux 运行日志 | `tmux capture-pane -pt cxu:frp -S -50` |
