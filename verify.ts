import { spawn, type Subprocess } from "bun";

const procs: Subprocess[] = [];
const killAll = () => { for (const p of procs) try { p.kill(); } catch {} };

async function waitFor(url: string, timeoutMs: number, opts?: RequestInit) {
  const t0 = Date.now();
  while (Date.now() - t0 < timeoutMs) {
    try {
      const r = await fetch(url, opts);
      return r;
    } catch { await Bun.sleep(300); }
  }
  throw new Error(`timeout waiting ${url}`);
}

try {
  procs.push(Bun.spawn(["bun", "demo/upstream.ts"], { cwd: "/home/vscode/projects/frp", env: { ...process.env, UPSTREAM_PORT: "43111" }, stdout: "ignore", stderr: "ignore" }));
  procs.push(Bun.spawn(["bun", "src/server.ts", "--http", "18080", "--ttl-hours", "24", "--db", "./data/verify-routes.json"],
    { cwd: "/home/vscode/projects/frp", env: { ...process.env, ADMIN_TOKEN: "test-admin", TUNNEL_TOKEN: "" }, stdout: "ignore", stderr: "ignore" }));
  await waitFor("http://127.0.0.1:18080/__health", 15000);
  console.log("PASS health");

  procs.push(Bun.spawn(["bun", "src/client.ts", "--server", "ws://127.0.0.1:18080/__tunnel", "--name", "colab1", "--upstream", "http://127.0.0.1:43111"],
    { cwd: "/home/vscode/projects/frp", env: process.env, stdout: "ignore", stderr: "ignore" }));

  // wait for ws registration
  let routes: any = null;
  for (let i = 0; i < 50; i++) {
    const r = await fetch("http://127.0.0.1:18080/__routes").then((x) => x.json()).catch(() => null);
    if (r?.routes?.some((x: any) => x.name === "colab1")) { routes = r; break; }
    await Bun.sleep(300);
  }
  if (!routes) throw new Error("client did not register");
  console.log("PASS ws-register", JSON.stringify(routes.routes));

  const hello = await fetch("http://127.0.0.1:18080/colab1/api/hello").then((x) => x.json());
  if (!hello?.ok) throw new Error("proxy via ws failed: " + JSON.stringify(hello));
  console.log("PASS ws-proxy", JSON.stringify(hello));

  const alloc = await fetch("http://127.0.0.1:18080/__admin/alloc", {
    method: "POST", headers: { "content-type": "application/json", "x-admin-token": "test-admin" },
    body: JSON.stringify({ name: "colab2" }),
  }).then((x) => x.json());
  if (!alloc?.ok || !alloc?.port || !alloc?.sshCmd?.includes("127.0.0.1:43110") || alloc?.publicUrl !== "https://us.cxu.lol/colab2/") {
    throw new Error("alloc failed: " + JSON.stringify(alloc));
  }
  console.log("PASS ssh-alloc", `name=${alloc.name} port=${alloc.port} sshCmd=${alloc.sshCmd}`);

  const offline = await fetch("http://127.0.0.1:18080/colab2/");
  const txt = await offline.text();
  if (offline.status !== 502 || !txt.includes("ssh tunnel offline")) throw new Error(`ssh offline path wrong: ${offline.status} ${txt}`);
  console.log("PASS ssh-offline-502");

  const forbidden = await fetch("http://127.0.0.1:18080/__admin/alloc", { method: "POST", headers: { "content-type": "application/json" }, body: "{}" });
  if (forbidden.status !== 403) throw new Error("admin auth missing");
  console.log("PASS admin-auth");

  console.log("ALL PASS");
} catch (e: any) {
  console.error("FAIL:", e?.message ?? e);
  process.exitCode = 1;
} finally {
  killAll();
  // cleanup verify db
  try { await Bun.$`rm -f /home/vscode/projects/frp/data/verify-routes.json`.quiet(); } catch {}
  setTimeout(() => process.exit(process.exitCode ?? 0), 300);
}
