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

function serverURL(): string {
  if (RAW.includes("://")) return RAW + (RAW.includes("?") ? "&" : "?") + `name=${encodeURIComponent(NAME)}`;
  // allow "host:port" or "host"
  const [h, p] = RAW.split(":");
  return `ws://${h}:${p ?? "8080"}/__tunnel?name=${encodeURIComponent(NAME)}`;
}
const URL_ = serverURL();
console.log(`[client] ${NAME} -> upstream ${UPSTREAM}`);
console.log(`[client] dialing ${URL_.replace(/token=[^&]*/g, "token=***")}`);

let ws: WebSocket | null = null;
let backoff = 1000;

function connect() {
  ws = new WebSocket(URL_);
  ws.binaryType = "arraybuffer";
  ws.onopen = () => {
    backoff = 1000;
    ws!.send(JSON.stringify({ type: "register", name: NAME, token: TOKEN || undefined }));
    console.log("[client] connected, registered");
  };
  ws.onmessage = async (ev) => {
    let m: any;
    try { m = JSON.parse(String(ev.data)); } catch { return; }
    if (m.type === "ping") { ws?.send(JSON.stringify({ type: "pong" })); return; }
    if (m.type === "registered") { console.log(`[client] route live: /${m.name}/`); return; }
    if (m.type === "error") { console.error(`[client] server error: ${m.error}`); return; }
    if (m.type !== "req") return;
    await handleReq(m as ReqMsg);
  };
  ws.onclose = () => {
    console.log(`[client] disconnected, retry in ${backoff}ms`);
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, 30_000);
  };
  ws.onerror = () => { try { ws?.close(); } catch {} };
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
  ws?.send(JSON.stringify({ type: "res", id: m.id, status, headers: resHeaders, bodyB64 }));
}

connect();
