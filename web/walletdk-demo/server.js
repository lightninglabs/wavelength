const http = require("http");
const fs = require("fs");
const path = require("path");

const host = process.env.HOST || "localhost";
const port = Number(process.env.PORT || 8081);
const root = path.join(__dirname, "dist");

const mimeTypes = {
  ".css": "text/css",
  ".html": "text/html",
  ".js": "application/javascript",
  ".json": "application/json",
  ".wasm": "application/wasm",
};

const server = http.createServer((req, res) => {
  res.setHeader("Cross-Origin-Opener-Policy", "same-origin");
  res.setHeader("Cross-Origin-Embedder-Policy", "require-corp");
  res.setHeader("Cross-Origin-Resource-Policy", "same-origin");

  const urlPath = decodeURIComponent(req.url.split("?")[0]);
  const relative = urlPath === "/" ? "index.html" : urlPath.slice(1);
  const filePath = path.normalize(path.join(root, relative));

  if (!filePath.startsWith(root)) {
    res.writeHead(403);
    res.end("forbidden");
    return;
  }

  fs.readFile(filePath, (err, content) => {
    if (err) {
      res.writeHead(err.code === "ENOENT" ? 404 : 500, {
        "Content-Type": "text/plain",
      });
      res.end(err.code === "ENOENT" ? "not found" : err.message);
      return;
    }

    res.writeHead(200, {
      "Content-Type": mimeTypes[path.extname(filePath)] || "application/octet-stream",
    });
    res.end(content);
  });
});

server.listen(port, host, () => {
  console.log(`walletdk demo: http://${host}:${port}/`);
});
