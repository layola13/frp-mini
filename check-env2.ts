import { spawn } from "bun";
async function sh(c: string) {
  const p = spawn(["bash", "-lc", c], { stdout: "pipe", stderr: "pipe" });
  const o = await new Response(p.stdout).text();
  const e = await new Response(p.stderr).text();
  await p.exited;
  return (o + "\n[STDERR]\n" + e).trim().slice(0, 5000);
}
console.log("=== sudo ===");
console.log(await sh("sudo -n true 2>&1 && echo HAS_SUDO || echo NO_SUDO; id 2>&1"));
console.log("=== nginx sites ===");
console.log(await sh("ls -la /etc/nginx/sites-enabled/ 2>&1; echo ---; cat /etc/nginx/sites-enabled/us.cxu.lol 2>&1 | head -120"));
console.log("=== probe 188.68.250.201 ===");
console.log(await sh("curl -sS --max-time 8 -o /dev/null -w 'v4-http:%{http_code}\\n' http://188.68.250.201/get.sh 2>&1; curl -skS --max-time 8 -o /dev/null -w 'v4-https:%{http_code}\\n' https://188.68.250.201/get.sh 2>&1; echo ---; curl -sS --max-time 8 http://188.68.250.201/get.sh 2>&1 | head -c 200; echo"));
console.log("=== probe proxy via resolve ===");
console.log(await sh("curl -sS --max-time 8 -o /dev/null -w 'proxy80:%{http_code} %{redirect_url}\\n' http://proxy.cxu.lol/get.sh 2>&1; curl -sS --max-time 8 http://proxy.cxu.lol/get.sh 2>&1 | head -c 200; echo"));
