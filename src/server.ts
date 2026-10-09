import { serve } from "bun";
import { existsSync, readFileSync, writeFileSync, appendFileSync, mkdirSync } from "node:fs";
import { createHash } from "node:crypto";

// us.cxu.lol/<name> -> remote 127.0.0.1:43110
// 双模式:
//  A) WS隧道 (推荐,类cloudflared): 远端跑 bun src/client.ts,无需公网IP
//  B) 免客户端ssh -R (零安装,仅需openssh): 远端 ssh -NTR 127.0.0.1:<port>:127.0.0.1:43110 tunnel@server

type CtlRegister = { type: "register"; name: string; token?: string };
type ProxyReq = { type: "req"; id: string; method: string; path: string; headers: Record<string,string>; bodyB64?: string };
type ProxyRes = { type: "res"; id: string; status: number; headers: Record<string,string>; bodyB64?: string };

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
const args = process.argv.slice(2);
const getArg = (k: string, d: string) => {
  const i = args.indexOf(`--${k}`);
  if (i >= 0) {
    const v = args[i + 1];
    if (v !== undefined && !v.startsWith("--")) return v; // 允许空串(关闭静态根)
  }
  return (process.env[k.toUpperCase()] ?? d);
};

const HTTP_PORT = Number(getArg("http", "8080"));
const EXPECT_TOKEN = process.env.TUNNEL_TOKEN ?? getArg("token", "");
const ADMIN_TOKEN = process.env.ADMIN_TOKEN ?? getArg("admin-token", EXPECT_TOKEN);
const TTL_MS = Number(getArg("ttl-hours", process.env.TTL_HOURS ?? "24")) * 3600_000;
const UPSTREAM_TIMEOUT_MS = 25_000;
const MAX_BODY = 10 * 1024 * 1024;
const SSH_BASE = Number(getArg("ssh-base-port", process.env.SSH_BASE_PORT ?? "14000"));
const SSH_MAX = Number(getArg("ssh-max-slots", "200"));
const DB_FILE = getArg("db", process.env.ROUTES_DB ?? "./data/routes.json");
const AUTH_KEYS = getArg("auth-keys", process.env.AUTH_KEYS ?? "/home/tunnel/.ssh/authorized_keys");
// A类绑定(静态/永久): 根路径 / 固定反代本地 43110,不走24h过期;远端动态名走 /<name>/ (B类,24h有效)
// STATIC_ROOT 为空则关闭根静态绑定;STATIC_BINDS 格式 "name=http://127.0.0.1:port,..." 可再加若干静态名
const STATIC_ROOT = getArg("static-root", process.env.STATIC_ROOT ?? "http://127.0.0.1:43110");
const STATIC_BINDS = new Map<string, string>();
for (const pair of (getArg("static-binds", process.env.STATIC_BINDS ?? "")).split(",").map((s) => s.trim()).filter(Boolean)) {
  const i = pair.indexOf("=");
  if (i > 0) STATIC_BINDS.set(pair.slice(0, i).trim().toLowerCase(), pair.slice(i + 1).trim().replace(/\/$/, ""));
}
// 对外域名(远端一键命令/落地页/enroll返回全用它): proxy.cxu.lol 独立部署,不碰 us.cxu.lol 主站 SPA
const PUBLIC_HOST = getArg("public-host", process.env.PUBLIC_HOST ?? "proxy.cxu.lol");
const SSH_HOST_DEF = getArg("ssh-host", process.env.SSH_HOST ?? PUBLIC_HOST);
const SSH_USER_DEF = getArg("ssh-user", process.env.SSH_USER ?? "tunnel");
// 系统保留名: 不可被 enroll/alloc 抢占(落地页与脚本直链优先)
const RESERVED = new Set(["get", "join", "get.sh", "join.sh", "remote-auto.sh", "__health", "__routes", "__enroll", "__tunnel", "__admin", ...STATIC_BINDS.keys()]);
const isReserved = (n: string) => RESERVED.has(n) || n.startsWith("__");
// 全自动 enroll: 动态口令 = SHA256(secret:step)[0:8], secret 默认取主机名 cxu.lol
// 远端按同样规则算出口令, POST 上报加密后的 pubkey, 服务端自动绑定 authorized_keys + 分配端口
const ENROLL_SECRET = process.env.ENROLL_SECRET ?? getArg("enroll-secret", "cxu.lol");
const ENROLL_STEP_SEC = Number(getArg("enroll-step-sec", process.env.ENROLL_STEP_SEC ?? "300"));
const stepOf = (t: number) => Math.floor(t / (ENROLL_STEP_SEC * 1000));
function enrollCode(step: number): string {
  return createHash("sha256").update(`${ENROLL_SECRET}:${step}`).digest("hex").slice(0, 8);
}
function verifyCode(code: string): boolean {
  const c = String(code ?? "").toLowerCase();
  const cur = stepOf(Date.now());
  for (const s of [cur - 1, cur, cur + 1]) if (enrollCode(s) === c) return true;
  return false;
}
// 解密远端 POST 来的 pubkey: openssl enc -aes-256-cbc -pbkdf2 -pass pass:$CODE -base64
// 等价 Node 实现: Salted__ + salt(8B) + AES-256-CBC, key/iv = EVP_BytesToKey(sha256链, pbkdf2-sha256-10k)
function evpBytesToKey(pass: Buffer, salt: Buffer): { key: Buffer; iv: Buffer } {
  let out = Buffer.alloc(0);
  let prev = Buffer.alloc(0);
  while (out.length < 48) {
    const h = createHash("sha256");
    h.update(Buffer.concat([prev, pass, salt]));
    prev = h.digest();
    out = Buffer.concat([out, prev]);
  }
  return { key: out.subarray(0, 32), iv: out.subarray(32, 48) };
}
function decryptEnrollPubkey(encB64: string, code: string): string {
  // 约定 enc = base64("Salted__" + salt8 + ciphertext), 与 openssl 互通
  const raw = Buffer.from(encB64, "base64");
  if (raw.subarray(0, 8).toString() !== "Salted__") throw new Error("bad enc format");
  const salt = raw.subarray(8, 16);
  const ct = raw.subarray(16);
  // 先试 pbkdf2(10k, sha256, 与新版 openssl 默认一致), 再回退单轮 md5/老版 EVP
  const { createDecipheriv, pbkdf2Sync } = require("node:crypto");
  const trySuites: Array<() => string> = [
    () => {
      const k = pbkdf2Sync(Buffer.from(code), salt, 10_000, 48, "sha256");
      const d = createDecipheriv("aes-256-cbc", k.subarray(0, 32), k.subarray(32, 48));
      return Buffer.concat([d.update(ct), d.final()]).toString("utf8");
    },
    () => {
      const { key, iv } = evpBytesToKey(Buffer.from(code), salt);
      const d = createDecipheriv("aes-256-cbc", key, iv);
      return Buffer.concat([d.update(ct), d.final()]).toString("utf8");
    },
  ];
  let lastErr: any = null;
  for (const fn of trySuites) {
    try {
      const s = fn().trim();
      if (/^(ssh-(ed25519|rsa|ecdsa|dss)|ecdsa-sha2-)/.test(s)) return s;
      lastErr = new Error("decrypted but not a pubkey");
    } catch (e) { lastErr = e; }
  }
  throw lastErr ?? new Error("decrypt fail");
}
const PUBKEY_RE = /^(ssh-(ed25519|rsa|ecdsa|dss)|ecdsa-sha2-nistp\d+)\s+[A-Za-z0-9+/=]+(\s+\S+)?$/;
function bindPubkey(name: string, pubkey: string): void {
  mkdirSync(AUTH_KEYS.split("/").slice(0, -1).join("/") || ".", { recursive: true });
  if (!existsSync(AUTH_KEYS)) writeFileSync(AUTH_KEYS, "");
  const cur = readFileSync(AUTH_KEYS, "utf8");
  const fp = createHash("sha256").update(pubkey.trim()).digest("hex").slice(0, 16);
  const marker = `# frp-auto ${name} ${fp}`;
  if (cur.includes(fp)) return; // 已绑定过
  const line = `restrict,port-forwarding,command="/bin/false" ${pubkey.trim()} ${marker}\n`;
  appendFileSync(AUTH_KEYS, line);
}

type Agent = { ws: import("bun").ServerWebSocket<{ name?: string }>; createdAt: number; expiresAt: number };
type TcpRoute = { name: string; port: number; pubkeyFp?: string; createdAt: number; expiresAt: number; via: "ssh" };
const agents = new Map<string, Agent>();
const tcpRoutes = new Map<string, TcpRoute>();
type PendingEntry = {
  resolve: (r: Response) => void;
  reject: (e: Error) => void;
  timer: Timer;
  name?: string;
};
const pending = new Map<string, PendingEntry>();
let reqSeq = 0;
const rid = () => `${Date.now().toString(36)}-${(reqSeq++).toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

// ---- 持久化 tcp路由(ssh -R分配表),server重启不丢,过期自动清 ----
function loadDb() {
  try {
    if (!existsSync(DB_FILE)) return;
    const j = JSON.parse(readFileSync(DB_FILE, "utf8"));
    const now = Date.now();
    for (const r of j.routes ?? []) {
      if (r.expiresAt > now && NAME_RE.test(r.name)) tcpRoutes.set(r.name, r);
    }
  } catch {}
}
function saveDb() {
  try {
    mkdirSync(DB_FILE.split("/").slice(0, -1).join("/") || ".", { recursive: true });
    writeFileSync(DB_FILE, JSON.stringify({ routes: [...tcpRoutes.values()] }, null, 2));
  } catch (e) { console.error("[db] save fail", e); }
}
loadDb();

function allocPort(): number | null {
  const used = new Set([...tcpRoutes.values()].map((r) => r.port));
  for (let i = 0; i < SSH_MAX; i++) {
    const p = SSH_BASE + i;
    if (!used.has(p)) return p;
  }
  return null;
}
function sweep() {
  const now = Date.now();
  let changed = false;
  for (const [n, a] of agents) if (a.expiresAt <= now) { try { a.ws.close(); } catch {} agents.delete(n); changed = true; console.log(`[expire] ws agent ${n}`); }
  for (const [n, r] of tcpRoutes) if (r.expiresAt <= now) { tcpRoutes.delete(n); changed = true; console.log(`[expire] ssh route ${n} :${r.port} (需远端断开 ssh -R,端口自动回收)`); }
  if (changed) saveDb();
}
setInterval(sweep, 60_000);

function findActiveTunnel(name: string): boolean {
  if (!name || !NAME_RE.test(name)) return false;
  if (STATIC_BINDS.has(name)) return true;
  if (agents.has(name)) return true;
  const t = tcpRoutes.get(name);
  if (t && t.expiresAt > Date.now()) return true;
  return false;
}

function resolveTunnelFromReq(req: Request): string | null {
  // 1. Try Referer header (e.g. https://proxy.cxu.lol/<name>/...)
  const referer = req.headers.get("referer");
  if (referer) {
    try {
      const refUrl = new URL(referer);
      const m = refUrl.pathname.match(/^\/([a-z0-9-]{1,32})(?:\/|$)/i);
      if (m) {
        const candidate = m[1].toLowerCase();
        if (findActiveTunnel(candidate)) return candidate;
      }
    } catch {}
  }
  // 2. Try Cookie header (cxu_tunnel=<name>)
  const cookie = req.headers.get("cookie");
  if (cookie) {
    const m = cookie.match(/(?:^|;\s*)cxu_tunnel=([a-z0-9-]{1,32})(?:;|$)/i);
    if (m) {
      const candidate = m[1].toLowerCase();
      if (findActiveTunnel(candidate)) return candidate;
    }
  }
  return null;
}

async function processResponse(r: Response, name?: string): Promise<Response> {
  const out = new Headers();
  r.headers.forEach((v, k) => {
    const lk = k.toLowerCase();
    if (["content-encoding", "content-length", "transfer-encoding", "connection"].includes(lk)) return;
    try { out.set(k, v); } catch {}
  });

  if (name) {
    out.set("Set-Cookie", `cxu_tunnel=${name}; Path=/; SameSite=Lax`);
    const loc = out.get("location");
    if (loc && loc.startsWith("/") && !loc.startsWith(`/${name}/`) && loc !== `/${name}`) {
      out.set("location", `/${name}${loc}`);
    }
  }

  const contentType = out.get("content-type") || "";
  if (name && contentType.includes("text/html")) {
    out.delete("content-security-policy");
    out.delete("content-security-policy-report-only");

    let html = await r.text();

    // 1. Inject <base href="/${name}/"> right after <head>
    if (/<head[^>]*>/i.test(html)) {
      html = html.replace(/(<head[^>]*>)/i, `$1\n    <base href="/${name}/" />`);
    }

    // 2. Rewrite root-relative links: href="/..." and src="/..." (not protocol-relative //)
    html = html.replace(/(href|src)=["']\/([^"']*)["']/gi, (match, attr, path) => {
      if (path.startsWith("/")) return match;
      if (path.startsWith(`${name}/`) || path === name) return match;
      const quote = match.includes(`"`) ? `"` : `'`;
      return `${attr}=${quote}/${name}/${path}${quote}`;
    });

    // 3. Inject websocket override & fetch/xhr interceptor helper before </head>
    const helperScript = `
    <script>
    window.__CODEX_UI_WEBSOCKET_URL__ = "/${name}/ws";
    (function() {
      var prefix = "/${name}";
      var origFetch = window.fetch;
      if (origFetch) {
        window.fetch = function(input, init) {
          try {
            if (typeof input === "string") {
              if (input.startsWith("/") && !input.startsWith("//") && !input.startsWith(prefix + "/") && input !== prefix) {
                input = prefix + input;
              }
            } else if (input && typeof input === "object" && typeof input.url === "string") {
              var u = new URL(input.url, window.location.href);
              if (u.origin === window.location.origin && u.pathname.startsWith("/") && !u.pathname.startsWith(prefix + "/") && u.pathname !== prefix) {
                input = new Request(prefix + u.pathname + u.search + u.hash, input);
              }
            }
          } catch (e) {}
          return origFetch.call(this, input, init);
        };
      }
      var origOpen = window.XMLHttpRequest && window.XMLHttpRequest.prototype.open;
      if (origOpen) {
        window.XMLHttpRequest.prototype.open = function(method, url) {
          try {
            if (typeof url === "string" && url.startsWith("/") && !url.startsWith("//") && !url.startsWith(prefix + "/") && url !== prefix) {
              url = prefix + url;
            }
          } catch (e) {}
          return origOpen.apply(this, arguments);
        };
      }
    })();
    </script>
`;
    if (/<\/head>/i.test(html)) {
      html = html.replace(/<\/head>/i, `${helperScript}</head>`);
    } else {
      html = helperScript + html;
    }

    return new Response(html, { status: r.status, headers: out });
  }

  return new Response(await r.arrayBuffer(), { status: r.status, headers: out });
}

function hopHeaders(h: Headers): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of h) {
    const lk = k.toLowerCase();
    if (lk === "host" || lk === "connection" || lk === "content-length") continue;
    out[k] = v;
  }
  return out;
}

async function forwardViaAgent(name: string, req: Request, fwdPath: string, tunnelName?: string): Promise<Response> {
  const a = agents.get(name);
  if (!a) return new Response(`agent '${name}' offline`, { status: 502 });
  const id = rid();
  const buf = req.method !== "GET" && req.method !== "HEAD" ? Buffer.from(await req.arrayBuffer()) : Buffer.alloc(0);
  if (buf.length > MAX_BODY) return new Response("body too large", { status: 413 });
  const msg: ProxyReq = { type: "req", id, method: req.method, path: fwdPath, headers: hopHeaders(req.headers), bodyB64: buf.length ? buf.toString("base64") : undefined };
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { pending.delete(id); reject(new Error("upstream timeout")); }, UPSTREAM_TIMEOUT_MS);
    pending.set(id, { resolve, reject, timer, name: tunnelName ?? name });
    a.ws.send(JSON.stringify(msg));
  });
}

// B) ssh -R模式: 本机127.0.0.1:<port>已由sshd转发到远端43110,直接HTTP反代即可,真正的"端口反代"
async function forwardViaLocalPort(port: number, req: Request, fwdPath: string, name?: string): Promise<Response> {
  const url = `http://127.0.0.1:${port}${fwdPath}`;
  const headers = new Headers();
  for (const [k, v] of req.headers) {
    const lk = k.toLowerCase();
    if (["host", "connection", "content-length"].includes(lk)) continue;
    try { headers.set(k, v); } catch {}
  }
  const buf = req.method !== "GET" && req.method !== "HEAD" ? Buffer.from(await req.arrayBuffer()) : undefined;
  if (buf && buf.length > MAX_BODY) return new Response("body too large", { status: 413 });
  try {
    const r = await fetch(url, { method: req.method, headers, body: buf as any, redirect: "manual" });
    return await processResponse(r, name);
  } catch (e: any) {
    return new Response(`ssh tunnel offline (port ${port}无监听,远端ssh -R断开了吗?): ${e?.message ?? e}`, { status: 502 });
  }
}

async function forwardViaUrl(upstreamBase: string, req: Request, fwdPath: string, name?: string): Promise<Response> {
  const base = upstreamBase.replace(/\/$/, "");
  const url = `${base}${fwdPath.startsWith("/") ? fwdPath : "/" + fwdPath}`;
  const headers = new Headers();
  for (const [k, v] of req.headers) {
    const lk = k.toLowerCase();
    if (["host", "connection", "content-length"].includes(lk)) continue;
    try { headers.set(k, v); } catch {}
  }
  try { headers.set("host", new URL(base).host); } catch {}
  const buf = req.method !== "GET" && req.method !== "HEAD" ? Buffer.from(await req.arrayBuffer()) : undefined;
  if (buf && buf.length > MAX_BODY) return new Response("body too large", { status: 413 });
  try {
    const r = await fetch(url, { method: req.method, headers, body: buf as any, redirect: "manual" });
    return await processResponse(r, name);
  } catch (e: any) {
    return new Response(`static upstream offline (${base}): ${e?.message ?? e}`, { status: 502 });
  }
}

async function forwardTunnel(name: string, req: Request, fwdPath: string): Promise<Response> {
  const sb = STATIC_BINDS.get(name);
  if (sb) return await forwardViaUrl(sb, req, fwdPath, name);
  if (agents.has(name)) return await forwardViaAgent(name, req, fwdPath, name);
  const t = tcpRoutes.get(name);
  if (t) {
    if (t.expiresAt <= Date.now()) { tcpRoutes.delete(name); saveDb(); return new Response("expired, re-alloc", { status: 410 }); }
    return await forwardViaLocalPort(t.port, req, fwdPath, name);
  }
  return new Response(`no such tunnel '${name}'`, { status: 404 });
}

async function applyRes(id: string, m: ProxyRes) {
  const p = pending.get(id);
  if (!p) return;
  pending.delete(id);
  clearTimeout(p.timer);
  const body = m.bodyB64 ? Buffer.from(m.bodyB64, "base64") : null;
  const headers = new Headers();
  for (const [k, v] of Object.entries(m.headers ?? {})) {
    if (["content-encoding", "content-length", "transfer-encoding", "connection"].includes(k.toLowerCase())) continue;
    try { headers.set(k, v); } catch {}
  }
  const baseRes = new Response(body, { status: m.status, headers });
  const processed = await processResponse(baseRes, p.name);
  p.resolve(processed);
}

function landingHtml(): string {
  return `<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${PUBLIC_HOST} 反代接入</title>
<style>body{font-family:system-ui,monospace;max-width:720px;margin:40px auto;padding:0 16px;line-height:1.7}code,pre{background:#f4f4f4;padding:2px 6px;border-radius:6px}pre{padding:12px;overflow:auto}button{font:inherit;padding:6px 12px;cursor:pointer;border-radius:6px;border:1px solid #ccc;background:#fff}</style></head><body>
<h2>远端接入：一条命令（零配置）</h2>
<p>远端主机（运行着 <code>127.0.0.1:43110</code>）直接执行下方命令即可接入，脚本会自动生成不重复机器名并建立专属隧道：</p>
<pre id="c">curl -fsSL http://${PUBLIC_HOST}/get.sh | bash</pre>
<button onclick="copy()">复制命令</button>
<script>
function copy(){
  navigator.clipboard.writeText("curl -fsSL http://${PUBLIC_HOST}/get.sh | bash").then(()=>alert("已复制，去远端粘贴执行"));
}
</script>
<p style="color:#666;font-size:14px">💡 如需自定义固定名称，可执行：<code>curl -fsSL http://${PUBLIC_HOST}/get.sh | NAME=自定义名字 bash</code></p>
<p>原理：脚本自动基于机器与密钥指纹分配唯一标识 → 动态口令加密认证 → 服务端自动绑定公钥与分配端口 → <code>ssh -R</code> 安全隧道，24h 有效。<a href="/__enroll/help">算法说明</a> · <a href="/get.sh">脚本直链</a></p>
<p style="color:#888">根路径 <code>/</code> 为 A 类静态绑定（本地 43110，常驻），动态隧道走 <code>/&lt;name&gt;/</code>（B 类，24h 有效），互不冲突。</p>
</body></html>`;
}

serve({
  port: HTTP_PORT,
  async fetch(req, server) {
    const url = new URL(req.url);
    const isWs = req.headers.get("upgrade")?.toLowerCase() === "websocket";

    if (url.pathname === "/__tunnel") {
      if (server.upgrade(req, { data: { kind: "agent_control" } })) return undefined;
      return new Response("websocket required", { status: 426 });
    }

    if (isWs) {
      const wsMatch = url.pathname.match(/^\/([A-Za-z0-9-]{1,32})(\/.*)?$/);
      let wsName: string | null = null;
      let wsFwdPath = url.pathname + url.search;
      if (wsMatch && findActiveTunnel(wsMatch[1].toLowerCase())) {
        wsName = wsMatch[1].toLowerCase();
        wsFwdPath = (wsMatch[2] || "/") + url.search;
      } else {
        wsName = resolveTunnelFromReq(req);
      }
      if (wsName) {
        const sb = STATIC_BINDS.get(wsName);
        const tr = tcpRoutes.get(wsName);
        let upstreamWsUrl: string | null = null;
        if (sb) {
          const u = new URL(sb);
          upstreamWsUrl = `ws://${u.host}${wsFwdPath}`;
        } else if (tr && tr.expiresAt > Date.now()) {
          upstreamWsUrl = `ws://127.0.0.1:${tr.port}${wsFwdPath}`;
        }
        if (upstreamWsUrl) {
          const upgraded = server.upgrade(req, {
            data: {
              kind: "proxy_client",
              targetUrl: upstreamWsUrl,
              subprotocols: req.headers.get("sec-websocket-protocol") || undefined,
            }
          });
          if (upgraded) return undefined;
          return new Response("websocket upgrade failed", { status: 500 });
        }
      }
      return new Response("websocket tunnel not found", { status: 404 });
    }

    if (url.pathname === "/__health") return Response.json({ ok: true, staticRoot: STATIC_ROOT || null, staticBinds: [...STATIC_BINDS.keys()], ws: [...agents.keys()], ssh: [...tcpRoutes.values()].map((r) => ({ name: r.name, port: r.port, expiresAt: r.expiresAt })) });
    // 落地页: /get 给远端一条命令, 无需手动拷贝脚本; / 本体留给 A 类静态绑定(本地43110)
    if ((url.pathname === "/get" || url.pathname === "/join") && req.method === "GET") {
      const accept = req.headers.get("accept") ?? "";
      if (accept.includes("text/html") || url.searchParams.has("html")) {
        return new Response(landingHtml(), { headers: { "content-type": "text/html; charset=utf-8" } });
      }
      return new Response(`远端一条命令: curl -fsSL http://${PUBLIC_HOST}/get.sh | bash\n浏览器打开 http://${PUBLIC_HOST}/get?html 查看\n`, { headers: { "content-type": "text/plain; charset=utf-8" } });
    }
    // 脚本直链: 远端 curl -fsSL http://proxy.cxu.lol/get.sh | bash
    if (url.pathname === "/get.sh" || url.pathname === "/join.sh" || url.pathname === "/remote-auto.sh") {
      try {
        const sh = readFileSync(new URL("../remote-auto.sh", import.meta.url), "utf8");
        return new Response(sh, { headers: { "content-type": "text/x-shellscript; charset=utf-8", "cache-control": "no-cache" } });
      } catch (e: any) {
        return new Response(`get.sh missing: ${e?.message ?? e}`, { status: 500 });
      }
    }
    // A 类静态绑定: / -> 本地 43110(常驻,不走24h过期);精确匹配才走静态, /<name>/ 仍走动态
    if ((url.pathname === "/" || url.pathname === "") && STATIC_ROOT) {
      if ((req.headers.get("accept") ?? "").includes("text/html") && url.searchParams.has("join")) {
        return new Response(landingHtml(), { headers: { "content-type": "text/html; charset=utf-8" } });
      }
      return await forwardViaUrl(STATIC_ROOT, req, "/" + url.search);
    }
    if (url.pathname === "/__routes" || url.pathname === "/") {
      const now = Date.now();
      return Response.json({
        ok: true, ttlHours: TTL_MS / 3600_000,
        routes: [
          ...[...agents.entries()].map(([n, a]) => ({ name: n, via: "ws", path: `/${n}/`, expiresInSec: Math.max(0, Math.round((a.expiresAt - now) / 1000)) })),
          ...[...tcpRoutes.entries()].map(([n, r]) => ({ name: n, via: "ssh", port: r.port, path: `/${n}/`, expiresInSec: Math.max(0, Math.round((r.expiresAt - now) / 1000)) })),
        ],
      });
    }
    // 自助 enroll(全自动,无需管理员): 远端按时间规则算动态口令,加密上报 pubkey,服务端自动绑定
    if (url.pathname === "/__enroll" && req.method === "POST") {
      const b: any = await req.json().catch(() => ({}));
      let name = String(b.name ?? "").toLowerCase();
      const code = String(b.code ?? "");
      if (!name) name = `node-${Math.random().toString(36).slice(2, 8)}`;
      if (!NAME_RE.test(name)) return new Response("bad name [a-z0-9-]", { status: 400 });
      if (isReserved(name)) return new Response("name reserved (get/join/静态名不可注册)", { status: 409 });
      if (!verifyCode(code)) return new Response("bad code (时钟差>5分钟? 按规则重算)", { status: 403 });
      if (agents.has(name)) return Response.json({ ok: false, error: "name taken by ws agent" }, { status: 409 });
      let enc = String(b.pubkey_enc ?? b.pubkey ?? "");
      if (enc && !enc.startsWith("U2FsdGVk")) {
        try {
          const once = Buffer.from(enc.trim(), "base64");
          if (once.subarray(0, 8).toString() !== "Salted__" && /^[A-Za-z0-9+/=\s]+$/.test(enc)) {
            const twice = Buffer.from(once.toString("utf8").trim(), "base64");
            if (twice.subarray(0, 8).toString() === "Salted__") enc = once.toString("utf8").trim();
          }
        } catch {}
      }
      let pubkey = String(b.pubkey ?? "");
      try {
        if (!pubkey && enc) pubkey = decryptEnrollPubkey(enc.trim(), code.toLowerCase());
      } catch (e: any) {
        return new Response(`decrypt pubkey fail: ${e?.message ?? e}`, { status: 400 });
      }
      pubkey = pubkey.trim();
      if (!PUBKEY_RE.test(pubkey)) return new Response("bad pubkey (要单行 openssh 公钥)", { status: 400 });

      const fp = createHash("sha256").update(pubkey.trim()).digest("hex").slice(0, 16);
      let assignedName = name;
      let r = tcpRoutes.get(assignedName);

      // 名称冲突保护: 若名称已被占用且不是同一公钥(不同机器)，服务端自动追加短随机后缀，确保永不覆盖
      if (r && r.expiresAt > Date.now()) {
        if (r.pubkeyFp && r.pubkeyFp !== fp) {
          let candidate = `${name}-${Math.random().toString(36).slice(2, 6)}`;
          while (tcpRoutes.has(candidate) && tcpRoutes.get(candidate)!.expiresAt > Date.now()) {
            candidate = `${name}-${Math.random().toString(36).slice(2, 6)}`;
          }
          assignedName = candidate;
          r = undefined;
        }
      }

      try { bindPubkey(assignedName, pubkey); } catch (e: any) {
        return new Response(`bind pubkey fail: ${e?.message ?? e}`, { status: 500 });
      }

      if (!r || r.expiresAt <= Date.now()) {
        const port = allocPort();
        if (!port) return new Response("no free ports", { status: 503 });
        r = { name: assignedName, port, pubkeyFp: fp, createdAt: Date.now(), expiresAt: Date.now() + TTL_MS, via: "ssh" };
        tcpRoutes.set(assignedName, r); saveDb();
      } else {
        r.pubkeyFp = fp;
        r.expiresAt = Date.now() + TTL_MS;
        saveDb();
      }
      const sshHost = getArg("ssh-host", process.env.SSH_HOST ?? SSH_HOST_DEF);
      const sshUser = getArg("ssh-user", process.env.SSH_USER ?? SSH_USER_DEF);
      console.log(`[enroll] ${assignedName} -> port ${r.port} (pubkey auto-bound)`);
      return Response.json({
        ok: true,
        name: assignedName,
        port: r.port,
        createdAt: r.createdAt,
        expiresAt: r.expiresAt,
        via: r.via,
        publicUrl: `https://${PUBLIC_HOST}/${assignedName}/`,
        sshCmd: `ssh -NTR 127.0.0.1:${r.port}:127.0.0.1:43110 -i ~/.ssh/tunnel ${sshUser}@${sshHost}`
      });
    }
    // 取号/算法说明(公开,口令本身仍需按时间算,方便远端脚本自解释)
    if (url.pathname === "/__enroll/help") {
      return Response.json({
        ok: true, host: PUBLIC_HOST,
        algo: "code = sha256(SECRET + ':' + floor(unix_now / STEP))[0:8]",
        secretHint: "cxu.lol", stepSec: ENROLL_STEP_SEC, skew: "±1 step",
        example_sh: 'CODE=$(echo -n "cxu.lol:$(( $(date +%s) / 300 ))" | sha256sum | cut -c1-8)',
        encrypt_sh: "openssl enc -aes-256-cbc -pbkdf2 -pass pass:$CODE -in ~/.ssh/tunnel.pub | base64 | tr -d '\\n'",
        post: `curl -XPOST https://${PUBLIC_HOST}/__enroll -d '{"name":"colab1","code":"$CODE","pubkey_enc":"..."}'`,
      });
    }
    // 管理API: 分配/释放 ssh -R 端口(24h有效)。Header: x-admin-token
    if (url.pathname === "/__admin/alloc" && req.method === "POST") {
      if (ADMIN_TOKEN && req.headers.get("x-admin-token") !== ADMIN_TOKEN) return new Response("forbidden", { status: 403 });
      const b: any = await req.json().catch(() => ({}));
      const name = String(b.name ?? url.searchParams.get("name") ?? "").toLowerCase();
      if (!NAME_RE.test(name)) return new Response("bad name [a-z0-9-]", { status: 400 });
      if (isReserved(name)) return new Response("name reserved (get/join/静态名不可注册)", { status: 409 });
      if (agents.has(name)) return Response.json({ ok: false, error: "name taken by ws agent" }, { status: 409 });
      let r = tcpRoutes.get(name);
      if (!r || r.expiresAt <= Date.now()) {
        const port = allocPort();
        if (!port) return new Response("no free ports", { status: 503 });
        r = { name, port, createdAt: Date.now(), expiresAt: Date.now() + TTL_MS, via: "ssh" };
        tcpRoutes.set(name, r); saveDb();
      } else { // 续期
        r.expiresAt = Date.now() + TTL_MS; saveDb();
      }
      const sshHost = getArg("ssh-host", process.env.SSH_HOST ?? SSH_HOST_DEF);
      const sshUser = getArg("ssh-user", process.env.SSH_USER ?? SSH_USER_DEF);
      return Response.json({ ok: true, ...r, publicUrl: `https://${PUBLIC_HOST}/${name}/`, sshCmd: `ssh -NTR 127.0.0.1:${r.port}:127.0.0.1:43110 ${sshUser}@${sshHost}` });
    }
    if (url.pathname === "/__admin/release" && req.method === "POST") {
      if (ADMIN_TOKEN && req.headers.get("x-admin-token") !== ADMIN_TOKEN) return new Response("forbidden", { status: 403 });
      const b: any = await req.json().catch(() => ({}));
      const name = String(b.name ?? "").toLowerCase();
      tcpRoutes.delete(name); saveDb();
      return Response.json({ ok: true });
    }

    // 显式路径匹配: /:name 或 /:name/*
    const m = url.pathname.match(/^\/([A-Za-z0-9-]{1,32})(\/.*)?$/);
    if (m) {
      const name = m[1].toLowerCase();
      if (findActiveTunnel(name)) {
        if (!m[2] && !url.pathname.endsWith("/")) {
          return new Response(null, {
            status: 302,
            headers: { Location: `/${name}/${url.search}` }
          });
        }
        const rest = m[2] || "/";
        const fwdPath = rest + url.search;
        try {
          return await forwardTunnel(name, req, fwdPath);
        } catch (e: any) {
          return new Response(`upstream error: ${e?.message ?? e}`, { status: 504 });
        }
      }
    }

    // 回退机制: 当请求没有 /<name>/ 前缀时(例如 /assets/*, /icons/*, /api/*, /manifest.webmanifest)
    // 根据 Referer 或 Cookie 识别所属隧道，自动转发
    const fallbackName = resolveTunnelFromReq(req);
    if (fallbackName) {
      const fwdPath = url.pathname + url.search;
      try {
        return await forwardTunnel(fallbackName, req, fwdPath);
      } catch (e: any) {
        return new Response(`upstream error: ${e?.message ?? e}`, { status: 504 });
      }
    }

    return new Response("not found", { status: 404 });
  },
  websocket: {
    open(ws) {
      const data = ws.data as any;
      if (data?.kind === "proxy_client") {
        try {
          const upstream = new WebSocket(data.targetUrl, data.subprotocols ? { protocols: [data.subprotocols] } : undefined);
          data.upstream = upstream;
          const queue: Array<string | ArrayBufferView | ArrayBuffer> = [];
          data.queue = queue;

          upstream.onopen = () => {
            while (queue.length > 0) {
              const msg = queue.shift()!;
              upstream.send(msg);
            }
          };
          upstream.onmessage = (event) => {
            try { ws.send(event.data); } catch {}
          };
          upstream.onclose = (event) => {
            try { ws.close(event.code, event.reason); } catch {}
          };
          upstream.onerror = () => {
            try { ws.close(1011, "upstream error"); } catch {}
          };
        } catch (e: any) {
          try { ws.close(1011, e?.message ?? "failed to connect upstream"); } catch {}
        }
        return;
      }
      data.name = undefined;
    },
    message(ws, raw) {
      const data = ws.data as any;
      if (data?.kind === "proxy_client") {
        const upstream = data.upstream as WebSocket | undefined;
        if (!upstream) return;
        if (upstream.readyState === WebSocket.OPEN) {
          upstream.send(raw);
        } else if (upstream.readyState === WebSocket.CONNECTING) {
          data.queue?.push(raw);
        }
        return;
      }
      let m: any;
      try { m = JSON.parse(String(raw)); } catch { return; }
      if (m.type === "register") {
        const name = String(m.name ?? "").toLowerCase();
        if (!NAME_RE.test(name)) { ws.send(JSON.stringify({ type: "error", error: "bad name" })); ws.close(); return; }
        if (EXPECT_TOKEN && m.token !== EXPECT_TOKEN) { ws.send(JSON.stringify({ type: "error", error: "bad token" })); ws.close(); return; }
        const old = agents.get(name);
        if (old && old.ws !== ws) try { old.ws.close(); } catch {}
        const now = Date.now();
        agents.set(name, { ws: ws as any, createdAt: old?.createdAt ?? now, expiresAt: now + TTL_MS });
        data.name = name;
        ws.send(JSON.stringify({ type: "registered", name, expiresAt: now + TTL_MS, publicUrl: `https://${PUBLIC_HOST}/${name}/` }));
        console.log(`[server] agent online: ${name} (24h有效,到期自动下线)`);
        return;
      }
      if (m.type === "pong") return;
      if (m.type === "res" && m.id) { applyRes(m.id, m as ProxyRes); return; }
    },
    close(ws) {
      const data = ws.data as any;
      if (data?.kind === "proxy_client") {
        const upstream = data.upstream as WebSocket | undefined;
        if (upstream && (upstream.readyState === WebSocket.OPEN || upstream.readyState === WebSocket.CONNECTING)) {
          try { upstream.close(); } catch {}
        }
        return;
      }
      const name = data?.name;
      if (name && agents.get(name)?.ws === (ws as any)) { agents.delete(name); console.log(`[server] agent offline: ${name}`); }
    },
  },
});

console.log(`[server] http+control ws on :${HTTP_PORT} ttl=${TTL_MS / 3600_000}h db=${DB_FILE}`);
console.log(`[server] ws mode : bun src/client.ts --server ws://127.0.0.1:${HTTP_PORT}/__tunnel --name colab1 --upstream http://127.0.0.1:43110`);
console.log(`[server] ssh mode : curl -XPOST -H 'x-admin-token: ***' localhost:${HTTP_PORT}/__admin/alloc -d '{"name":"colab1"}' -> ssh -NTR 127.0.0.1:<port>:127.0.0.1:43110 tunnel@host`);
setInterval(() => { for (const a of agents.values()) try { a.ws.send(JSON.stringify({ type: "ping" })); } catch {} }, 25_000);
