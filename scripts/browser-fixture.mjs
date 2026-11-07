// Local-only upstream for repeatable browser acceptance, independent of demo.db.
import http from "node:http";
const port = Number(process.argv[2] || 18083);
const server = http.createServer((request, response) => {
  const path = new URL(request.url, "http://127.0.0.1").pathname;
  response.setHeader("Content-Type", "application/json; charset=utf-8");
  if (path === "/slow") {
    const timer = setTimeout(() => response.end('{"status":"slow"}'), 2000);
    response.on("close", () => clearTimeout(timer));
  } else if (path === "/error") {
    response.writeHead(500).end('{"status":"error"}');
  } else if (path === "/redirect") {
    response.writeHead(302, { Location: "/ok" }).end();
  } else if (path === "/ok") {
    response.end('{"status":"ok"}');
  } else {
    response.writeHead(404).end('{"status":"not-found"}');
  }
});
server.listen(port, "127.0.0.1", () => console.log(`Fixture http://127.0.0.1:${port}`));
