'use strict';
// Loopback-only static fixture; API behavior is controlled per browser test.
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const root = path.resolve(__dirname, '../static');
http.createServer((req, res) => {
    const name = new URL(req.url, 'http://localhost').pathname;
    const file = name === '/' ? 'index.html' : name.slice(1);
    if (path.basename(file) !== file) {
        res.writeHead(404).end();
        return;
    }
    fs.readFile(path.join(root, file), (err, body) => {
        if (err) {
            res.writeHead(404).end();
            return;
        }
        res.setHeader(
            'Content-Type',
            {
                '.html': 'text/html',
                '.js': 'text/javascript',
                '.css': 'text/css',
                '.svg': 'image/svg+xml',
            }[path.extname(file)] || 'application/octet-stream',
        );
        res.setHeader(
            'Content-Security-Policy',
            "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; frame-src 'self' blob:; object-src 'none'; base-uri 'none'",
        );
        res.end(body);
    });
}).listen(19381, '127.0.0.1');
