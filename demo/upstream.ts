import { serve } from "bun";
const PORT = Number(process.env.UPSTREAM_PORT ?? 43110);
serve({
  port: PORT,
  fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === "/") {
      return new Response(
        `<h1>colab1 upstream ok</h1><p>prefix: ${req.headers.get("x-forwarded-prefix")}</p>` +
        `<p>try <a href="?x=1">query</a> or /api/hello</p>`,
        { headers: { "content-type": "text/html; charset=utf-8" } },
      );
    }
    if (url.pathname === "/api/hello") return Response.json({ ok: true, from: `upstream:${PORT}` });
    return new Response("upstream 404", { status: 404 });
  },
});
console.log(`[demo-upstream] listening on 127.0.0.1:${PORT}`);
