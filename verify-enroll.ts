import { $ } from "bun";

// 端到端验证 enroll 全自动流程:
// 1. 起 server(18081, 隔离db/keys) 2. 远端逻辑: 生成key -> 按时间规则算code -> openssl加密pubkey -> POST /__enroll
// 3. 断言: 返回port+sshCmd, authorized_keys 已绑定, 错误code被403

const HTTP = "18081";
const DB = "./data/verify-enroll-routes.json";
const KEYS = "./data/verify-authorized_keys";
try { await Bun.$`rm -f ${DB} ${KEYS}`.quiet(); } catch {}

const server = Bun.spawn(["bun", "src/server.ts", "--http", HTTP, "--ttl-hours", "24", "--db", DB,
  "--auth-keys", KEYS, "--enroll-secret", "cxu.lol", "--enroll-step-sec", "300"],
  { cwd: "/home/vscode/projects/frp", env: { ...process.env, ADMIN_TOKEN: "test-admin", TUNNEL_TOKEN: "" }, stdout: "ignore", stderr: "ignore" });
const kill = () => { try { server.kill(); } catch {} };

async function waitFor(url: string, ms: number) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    try { const r = await fetch(url); if (r.ok) return; } catch {}
    await Bun.sleep(300);
  }
  throw new Error("timeout " + url);
}

try {
  await waitFor(`http://127.0.0.1:${HTTP}/__health`, 15000);
  console.log("PASS health");

  // 远端: 生成临时key
  await $`rm -rf ./data/verify-remote && mkdir -p ./data/verify-remote && ssh-keygen -t ed25519 -f ./data/verify-remote/tunnel -N "" -C colab9 -q`.cwd("/home/vscode/projects/frp").quiet();
  const pub = (await Bun.file("/home/vscode/projects/frp/data/verify-remote/tunnel.pub").text()).trim();
  console.log("PASS keygen", pub.slice(0, 30) + "...");

  // 远端: 按时间规则算 code (与服务端同一规则: sha256("cxu.lol:step")[0:8])
  const { createHash } = await import("node:crypto");
  const step = Math.floor(Date.now() / 300_000);
  const code = createHash("sha256").update(`cxu.lol:${step}`).digest("hex").slice(0, 8);

  // 远端: openssl 加密 pubkey (与 remote-auto.sh 同命令)
  const encProc = Bun.spawn(["bash", "-lc", "openssl enc -aes-256-cbc -pbkdf2 -pass pass:$0 -in data/verify-remote/tunnel.pub | base64 | tr -d '\\n'", code],
    { cwd: "/home/vscode/projects/frp", stdout: "pipe" });
  const enc = (await new Response(encProc.stdout).text()).trim();
  if (!enc.startsWith("U2FsdGVk")) throw new Error("openssl enc output unexpected: " + enc.slice(0, 20));

  const resp = await fetch(`http://127.0.0.1:${HTTP}/__enroll`, {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ name: "colab9", code, pubkey_enc: enc }),
  });
  const j: any = await resp.json();
  if (!resp.ok || !j?.ok || !j?.port || !j?.sshCmd?.includes("-NTR") || !j?.publicUrl?.endsWith("/colab9/")) {
    throw new Error("enroll fail: " + resp.status + " " + JSON.stringify(j));
  }
  console.log(`PASS enroll name=colab9 port=${j.port}`);

  const keys = await Bun.file("/home/vscode/projects/frp/" + KEYS.replace("./", "")).text();
  if (!keys.includes("frp-auto colab9") || !keys.includes("restrict,port-forwarding")) throw new Error("authorized_keys not bound");
  console.log("PASS keys-bound");

  // 错误口令必须 403
  const bad = await fetch(`http://127.0.0.1:${HTTP}/__enroll`, {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ name: "colabX", code: "deadbeef", pubkey_enc: enc }),
  });
  if (bad.status !== 403) throw new Error("bad code not rejected: " + bad.status);
  console.log("PASS bad-code-403");

  // help 公开算法说明
  const help: any = await fetch(`http://127.0.0.1:${HTTP}/__enroll/help`).then((r) => r.json());
  if (!help?.algo?.includes("sha256")) throw new Error("help missing");
  console.log("PASS enroll-help");

  console.log("ALL ENROLL PASS");
} catch (e: any) {
  console.error("FAIL:", e?.message ?? e);
  process.exitCode = 1;
} finally {
  kill();
  try { await Bun.$`rm -f ./data/verify-enroll-routes.json ./data/verify-authorized_keys && rm -rf ./data/verify-remote`.cwd("/home/vscode/projects/frp").quiet(); } catch {}
  setTimeout(() => process.exit(process.exitCode ?? 0), 300);
}
