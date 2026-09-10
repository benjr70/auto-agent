#!/usr/bin/env python3
"""PROTOTYPE fixture app. One process, one port, in-memory state."""
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
ITEMS = []
class H(BaseHTTPRequestHandler):
    def _send(self, code, body, ctype="application/json"):
        data = body.encode() if isinstance(body, str) else json.dumps(body).encode()
        self.send_response(code); self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_GET(self):
        if self.path == "/api/health": return self._send(200, {"status": "ok"})
        if self.path == "/api/items": return self._send(200, ITEMS)
        if self.path == "/": return self._send(200, "<h1>fixture</h1><ul id=items>" + "".join(f"<li>{i}</li>" for i in ITEMS) + "</ul>", "text/html")
        self._send(404, {"error": "not found"})
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0)); ITEMS.append(self.rfile.read(n).decode()); self._send(201, {"count": len(ITEMS)})
    def log_message(self, *a): pass
port = int(sys.argv[1]); HTTPServer(("127.0.0.1", port), H).serve_forever()
