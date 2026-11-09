# Un servicio del "host" en su IP pública: el invitado no debe alcanzarlo.
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header("Content-Length", "11"); self.end_headers(); self.wfile.write(b"host-propio")
    def log_message(self, *a): pass
http.server.HTTPServer(("198.51.100.2", 7070), H).serve_forever()
