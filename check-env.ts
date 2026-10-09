import { spawn } from "bun";
async function sh(c: string) {
  const p = spawn(["bash", "-lc", c], { stdout: "pipe", stderr: "pipe" });
  const o = await new Response(p.stdout).text();
  const e = await new Response(p.stderr).text();
  await p.exited;
  return (o + "\n" + e).trim().slice(0, 4000);
}
console.log("=== hostname/ip ===");
console.log(await sh("hostname; echo ---; hostname -I 2>&1; echo ---; curl -sS --max-time 8 ifconfig.me 2>&1 || curl -sS --max-time 8 ifconfig.io 2>&1"));
console.log("=== dns ===");
console.log(await sh("getent hosts proxy.cxu.lol us.cxu.lol 2>&1; echo ---; cat /etc/hosts 2>&1"));
console.log("=== nginx ===");
console.log(await sh("which nginx 2>&1; echo ---; nginx -t 2>&1; echo ---; ls /etc/nginx/conf.d/ 2>&1; ls /etc/nginx/sites-enabled/ 2>&1"));
console.log("=== ports ===");
console.log(await sh("ss -tlnp 2>&1 | head -30"));
console.log("=== certs ===");
console.log(await sh("ls /etc/letsencrypt/live/ 2>&1; echo ---; which certbot 2>&1; certbot --version 2>&1 || echo no-certbot"));
console.log("=== local health ===");
console.log(await sh("curl -sS --max-time 5 http://127.0.0.1:8080/__health 2>&1; echo; curl -sS --max-time 5 http://127.0.0.1:8080/get.sh 2>&1 | head -5"));
console.log("=== remote https ===");
console.log(await sh("curl -sSI --max-time 10 https://proxy.cxu.lol/get.sh 2>&1 | head -30; echo ===; curl -sS --max-time 10 https://proxy.cxu.lol/get.sh 2>&1 | head -c 300; echo"));
