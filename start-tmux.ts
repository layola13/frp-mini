import { $ } from "bun";
// check tmux + start frp server in tmux session `frp`
const session = "frp";
try {
  const which = await $`which tmux bun`.text().catch(() => "");
  console.log("[check]", which.trim());
} catch (e: any) {
  console.log("[check] which fail:", e?.message);
}
try {
  const ls = await $`tmux ls`.text();
  console.log("[tmux ls]\n" + ls);
} catch (e: any) {
  console.log("[tmux ls] none/err:", (e?.stderr ?? e?.message ?? "").toString().slice(0, 500));
}
// kill old session if exists (idempotent restart)
await $`tmux kill-session -t ${session}`.quiet().catch(() => {});
// start new detached session: frp server on :8080, ttl 24h
// TUNNEL_TOKEN/ADMIN_TOKEN via env, defaults for local run
const env = {
  ...process.env,
  ADMIN_TOKEN: process.env.ADMIN_TOKEN ?? "secret",
  TUNNEL_TOKEN: process.env.TUNNEL_TOKEN ?? "",
};
const cmd = `cd /home/vscode/projects/frp && TUNNEL_TOKEN=${JSON.stringify(env.TUNNEL_TOKEN)} ADMIN_TOKEN=${JSON.stringify(env.ADMIN_TOKEN)} bun src/server.ts --http 8080 --ttl-hours 24 --db ./data/routes.json --public-host proxy.cxu.lol --static-root ""`;
console.log("[start]", cmd);
await $`tmux new-session -d -s ${session} -c /home/vscode/projects/frp ${cmd}`.text().then((t) => console.log("[tmux new]", t)).catch(async (e: any) => {
  console.log("[tmux new fail]", (e?.stderr ?? e?.message ?? "").toString().slice(0, 2000));
  // fallback: nohup detached via setsid
  console.log("[fallback] setsid nohup");
  const p = Bun.spawn(["bash", "-lc", `nohup ${cmd} > /home/vscode/projects/frp/data/server.log 2>&1 & echo $!`], { stdout: "pipe" });
  const out = await new Response(p.stdout).text();
  console.log("[fallback pid]", out);
});
await Bun.sleep(1500);
try {
  const cap = await $`tmux capture-pane -p -t ${session}`.text();
  console.log("[pane]\n" + cap.slice(0, 3000));
} catch (e: any) {
  console.log("[pane fail]", (e?.stderr ?? e?.message ?? "").toString().slice(0, 500));
}
try {
  const h = await fetch("http://127.0.0.1:8080/__health").then((r) => r.text());
  console.log("[health]", h.slice(0, 1000));
} catch (e: any) {
  console.log("[health fail]", e?.message);
}
