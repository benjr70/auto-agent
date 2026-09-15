#!/usr/bin/env python3
"""PROTOTYPE fixture app for the Xvfb tour: one page with a sticky bottom bar."""
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
PAGE = """<!doctype html><html><head><meta name=viewport content="width=device-width">
<style>body{margin:0;font-family:sans-serif;background:#f4f1ea;color:#222}
header{background:#7a2e1d;color:#fff;padding:16px;font-size:22px}
main{padding:16px}.card{background:#fff;border-radius:8px;padding:12px;margin:0 0 12px;box-shadow:0 1px 3px rgba(0,0,0,.2)}
.big{font-size:48px;font-weight:700;color:#c0392b}
nav{position:fixed;bottom:0;left:0;right:0;display:flex;background:#222;color:#fff}
nav div{flex:1;text-align:center;padding:14px 0;border-top:3px solid transparent}nav .on{border-top-color:#e67e22}
.tall{height:1400px}</style></head><body>
<header>Fixture Smoker</header><main>
<div class=card><div>Chamber</div><div class=big>225&deg;F</div></div>
<div class=card><div>Meat probe 1</div><div class=big>147&deg;F</div></div>
<div class=card>Items: <span id=n>0</span> &mdash; emoji test &#x1F525;&#x1F356; &mdash; Wg fi &#8211; 0O1l</div>
<div class=tall></div></main>
<nav><div class=on>Smoke</div><div>History</div><div>Review</div><div>Settings</div></nav>
</body></html>"""
class H(BaseHTTPRequestHandler):
    def _send(self, code, body, ctype="application/json"):
        data = body.encode() if isinstance(body, str) else json.dumps(body).encode()
        self.send_response(code); self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_GET(self):
        if self.path == "/api/health": return self._send(200, {"status": "ok"})
        if self.path == "/": return self._send(200, PAGE, "text/html")
        self._send(404, {"error": "not found"})
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
