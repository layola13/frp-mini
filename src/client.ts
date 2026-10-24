type ReqMsg = {
  type: "req"; id: string; method: string; path: string;
  headers: Record<string, string>; bodyB64?: string;
};

const args = process.argv.slice(2);
const getArg = (k: string, d: string) => {
  const i = args.indexOf(`--${k}`);
  return i >= 0 && args[i + 1] ? args[i + 1] : (process.env[k.toUpperCase()] ?? d);
};

// server: ws(s)://host:port/__tunnel  e.g. ws://127.0.0.1:8080/__tunnel
const RAW = getArg("server", "ws://127.0.0.1:8080/__tunnel");
const NAME = (getArg("name", "colab1") || "colab1").toLowerCase();
const UPSTREAM = (getArg("upstream", "http://127.0.0.1:43110") || "").replace(/\/$/, "");
const TOKEN = process.env.TUNNEL_TOKEN ?? getArg("token", "");
// TCP 公网端口转发: --tcp-target host:port [--tcp-port 期望公网端口]
const TCP_TARGET = getArg("tcp-target", process.env.TCP_TARGET ?? "");
const TCP_PORT = Number(getArg("tcp-port", process.env.TCP_PORT ?? "0")) || 0;
const TCP_ENABLED = TCP_TARGET.length > 0;

function splitHostPort(s: string): [string, number] {
  const i = s.lastIndexOf(":");
  if (i <= 0) return [s, 0];
  return [s.slice(0, i), Number(s.slice(i + 1)) || 0];
}

function serverURL(): string {
  if (RAW.includes("://")) return RAW + (RAW.includes("?") ? "&" : "?") + `name=${encodeURIComponent(NAME)}`;
  // allow "host:port" or "host"
  const [h, p] = RAW.split(":");
  return `ws://${h}:${p ?? "8080"}/__tunnel?name=${encodeURIComponent(NAME)}`;
}
let currentURL = serverURL();
console.log(`[client] ${NAME} -> upstream ${UPSTREAM}`);
console.log(`[client] dialing ${currentURL.replace(/token=[^&]*/g, "token=***")}`);

let ws: WebSocket | null = null;
let backoff = 1000;
function sendWS(msg: any) {
  try { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg)); } catch {}
}
const activeWs = new Map<string, WebSocket>();
const tcpConns = new Map<string, any>();

function connect() {
  ws = new WebSocket(currentURL);
  ws.binaryType = "arraybuffer";
  ws.onopen = () => {
    backoff = 1000;
    ws!.send(JSON.stringify({
      type: "register", name: NAME, token: TOKEN || undefined,
      tcp: TCP_ENABLED ? { port: TCP_PORT || undefined } : undefined,
    }));
    console.log("[client] connected, registered");
  };
  ws.onmessage = async (ev) => {
    let m: any;
    try { m = JSON.parse(String(ev.data)); } catch { return; }
    if (m.type === "ping") { sendWS({ type: "pong" }); return; }
    if (m.type === "registered") {
      console.log(`[client] route live: /${m.name}/`);
      if (m.tcpPort) console.log(`[client] tcp public: ${m.tcpHost ?? ""}:${m.tcpPort} -> ${TCP_TARGET}`);
      return;
    }
    if (m.type === "error") { console.error(`[client] server error: ${m.error}`); return; }

    // 1. HTTP 转发
    if (m.type === "req") {
      await handleReq(m as ReqMsg);
      return;
    }

    // 2. WebSocket 代理转发 (为前端如 /ws 提供全双工实时通信)
    if (m.type === "ws_open") {
      const wsTargetUrl = UPSTREAM.replace(/^http:\/\//, "ws://").replace(/^https:\/\//, "wss://") + m.path;
      try {
        const upstreamWs = new WebSocket(wsTargetUrl, m.protocols ? { protocols: [m.protocols] } : undefined);
        upstreamWs.binaryType = "arraybuffer";
        activeWs.set(m.connId, upstreamWs);

        upstreamWs.onmessage = (event) => {
          if (typeof event.data === "string") {
            sendWS({ type: "ws_data", connId: m.connId, data: event.data, isBinary: false });
          } else {
            const b64 = Buffer.from(event.data).toString("base64");
            sendWS({ type: "ws_data", connId: m.connId, data: b64, isBinary: true });
          }
        };
        upstreamWs.onclose = (event) => {
          sendWS({ type: "ws_close", connId: m.connId, code: event.code, reason: event.reason });
          activeWs.delete(m.connId);
        };
        upstreamWs.onerror = () => {
          sendWS({ type: "ws_close", connId: m.connId, code: 1011, reason: "upstream ws error" });
          activeWs.delete(m.connId);
        };
      } catch (err: any) {
        sendWS({ type: "ws_close", connId: m.connId, code: 1011, reason: err?.message });
      }
      return;
    }

    if (m.type === "ws_data") {
      const uws = activeWs.get(m.connId);
      if (uws && uws.readyState === WebSocket.OPEN) {
        if (m.isBinary && m.data) {
          uws.send(Buffer.from(m.data, "base64"));
        } else {
          uws.send(m.data ?? "");
        }
      }
      return;
    }

    if (m.type === "ws_close") {
      const uws = activeWs.get(m.connId);
      if (uws) {
        try { uws.close(m.code || 1000, m.reason || ""); } catch {}
        activeWs.delete(m.connId);
      }
      return;
    }

    // 3. 原始 TCP 转发 (公网端口 <-> 本地 --tcp-target)
    if (m.type === "tcp_open") {
      await handleTcpOpen(m);
      return;
    }
    if (m.type === "tcp_data") {
      const sock = tcpConns.get(m.connId);
      if (sock && m.data) { try { sock.write(Buffer.from(m.data, "base64")); } catch {} }
      return;
    }
    if (m.type === "tcp_close") {
      const sock = tcpConns.get(m.connId);
      if (sock) {
        tcpConns.delete(m.connId);
        try { sock.end(); } catch { try { sock.terminate?.(); } catch {} }
      }
      return;
    }
  };
  ws.onclose = (ev: any) => {
    if (ev?.code && ev.code !== 1000) console.log(`[client] ws closed code=${ev.code} reason=${ev?.reason ?? ""}`);
    for (const [id, uws] of activeWs.entries()) {
      try { uws.close(); } catch {}
    }
    activeWs.clear();
    for (const [, sock] of tcpConns.entries()) { try { sock.end(); } catch {} }
    tcpConns.clear();
    console.log(`[client] disconnected, retry in ${backoff}ms`);
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, 30_000);
  };
  ws.onerror = (e: any) => {
    console.log("[client] ws error:", e?.message ?? e);
    if (currentURL.startsWith("ws://") && !currentURL.includes("127.0.0.1") && !currentURL.includes("localhost")) {
      console.log("[client] ws connection failed (redirect?), upgrading to wss:// ...");
      currentURL = currentURL.replace(/^ws:\/\//, "wss://");
    }
    try { ws?.close(); } catch {}
  };
}

async function handleTcpOpen(m: any) {
  const connId: string = m.connId;
  const [host, port] = splitHostPort(TCP_TARGET);
  if (!host || !port) {
    sendWS({ type: "tcp_close", connId, error: `bad tcp target: ${TCP_TARGET}` });
    return;
  }
  let closed = false;
  try {
    const sock = await Bun.connect({
      hostname: host,
      port,
      socket: {
        open(s) { tcpConns.set(connId, s); },
        data(_s, data) {
          sendWS({ type: "tcp_data", connId, data: Buffer.from(data).toString("base64") });
        },
        close() {
          closed = true;
          if (!tcpConns.delete(connId)) return;
          sendWS({ type: "tcp_close", connId });
        },
        error(_s, err) {
          closed = true;
          if (!tcpConns.delete(connId)) return;
          sendWS({ type: "tcp_close", connId, error: err?.message });
        },
      },
    });
    if (!closed) tcpConns.set(connId, sock);
  } catch (e: any) {
    sendWS({ type: "tcp_close", connId, error: e?.message ?? String(e) });
  }
}

async function handleReq(m: ReqMsg) {
  const headers = new Headers();
  for (const [k, v] of Object.entries(m.headers ?? {})) {
    try { headers.set(k, v); } catch {}
  }
  headers.set("host", new URL(UPSTREAM).host);
  headers.set("x-forwarded-prefix", `/${NAME}`);
  let body: Buffer | undefined;
  if (m.bodyB64) body = Buffer.from(m.bodyB64, "base64");
  let status = 502;
  const resHeaders: Record<string, string> = {};
  let bodyB64: string | undefined;
  try {
    const r = await fetch(UPSTREAM + m.path, {
      method: m.method, headers, body: body as any,
    });
    status = r.status;
    r.headers.forEach((v, k) => {
      const lk = k.toLowerCase();
      if (["content-encoding", "content-length", "transfer-encoding", "connection"].includes(lk)) return;
      resHeaders[k] = v;
    });
    const buf = Buffer.from(await r.arrayBuffer());
    if (buf.length) bodyB64 = buf.subarray(0, 10 * 1024 * 1024).toString("base64");
  } catch (e: any) {
    resHeaders["content-type"] = "text/plain";
    bodyB64 = Buffer.from(`upstream ${UPSTREAM} unreachable: ${e?.message ?? e}`).toString("base64");
  }
  sendWS({ type: "res", id: m.id, status, headers: resHeaders, bodyB64 });
}

connect();
