// A static file server for the site during tests.
//
// Node's own http module rather than a dependency: the site is plain files,
// and this is thirty lines.

import { createReadStream, readFileSync, statSync } from 'node:fs';
import { createServer } from 'node:http';
import path from 'node:path';

const [, , portArg, rootArg, apiOrigin] = process.argv;
const port = Number(portArg);
const root = path.resolve(rootArg);

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.webmanifest': 'application/manifest+json',
  '.svg': 'image/svg+xml',
  '.webm': 'audio/webm',
  '.m4a': 'audio/mp4',
};

createServer((req, res) => {
  const requested = decodeURIComponent(new URL(req.url, 'http://x').pathname);
  const filePath = path.join(root, requested === '/' ? 'index.html' : requested);

  // Never serve outside the site directory, even in a test.
  if (!filePath.startsWith(root)) {
    res.writeHead(403).end('forbidden');
    return;
  }

  try {
    if (!statSync(filePath).isFile()) throw new Error('not a file');
  } catch {
    res.writeHead(404).end('not found');
    return;
  }

  const contentType = TYPES[path.extname(filePath)] ?? 'application/octet-stream';

  // Point the pages at this run's API. The meta tag is the same mechanism a
  // real deployment uses, so the tests exercise the production code path
  // rather than a special case; only the value differs.
  if (apiOrigin && path.extname(filePath) === '.html') {
    const html = readFileSync(filePath, 'utf8').replace(
      /<meta name="fns-api-base" content="[^"]*">/,
      `<meta name="fns-api-base" content="${apiOrigin}">`,
    );
    res.writeHead(200, { 'Content-Type': contentType, 'Cache-Control': 'no-store' });
    res.end(html);
    return;
  }

  res.writeHead(200, { 'Content-Type': contentType, 'Cache-Control': 'no-store' });
  createReadStream(filePath).pipe(res);
}).listen(port, '127.0.0.1');
