# Internet de mentira para el e2e del egress: DNS en 1.1.1.1:53 (udp y tcp),
# HTTP en :8080 y un contador UDP en :9999 (GET /cuenta lo da y lo pone a 0).
import socket, struct, threading, http.server, sys

ZONA = {"permitido.test": "198.51.100.1", "hostip.test": "198.51.100.2"}
cuenta = 0
mu = threading.Lock()

def responder(q):
    i, nombre = 12, []
    while q[i]:
        nombre.append(q[i+1:i+1+q[i]].decode()); i += q[i] + 1
    fin = i + 5
    n = ".".join(nombre).lower()
    ip = ZONA.get(n)
    cab = q[:2] + (b"\x81\x80" if ip else b"\x81\x83") + q[4:6] + (b"\x00\x01" if ip else b"\x00\x00") + b"\x00\x00\x00\x00"
    r = cab + q[12:fin]
    if ip:
        r += b"\xc0\x0c\x00\x01\x00\x01" + struct.pack(">I", 300) + b"\x00\x04" + socket.inet_aton(ip)
    return r

def dns_udp():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("1.1.1.1", 53))
    while True:
        q, a = s.recvfrom(4096)
        try: s.sendto(responder(q), a)
        except Exception: pass

def dns_tcp():
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("1.1.1.1", 53)); s.listen(16)
    while True:
        c, _ = s.accept()
        try:
            l = struct.unpack(">H", c.recv(2))[0]; q = b""
            while len(q) < l: q += c.recv(l - len(q))
            r = responder(q); c.sendall(struct.pack(">H", len(r)) + r)
        except Exception: pass
        c.close()

def contador():
    global cuenta
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("0.0.0.0", 9999))
    s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 8 << 20)
    while True:
        s.recv(2048)
        with mu: cuenta += 1

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global cuenta
        if self.path == "/cuenta":
            with mu: c, cuenta = cuenta, 0
            body = str(c).encode()
        else:
            body = b"fakeinet " + self.server.server_address[0].encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass

for f in (dns_udp, dns_tcp, contador):
    threading.Thread(target=f, daemon=True).start()
http.server.ThreadingHTTPServer(("0.0.0.0", 8080), H).serve_forever()
