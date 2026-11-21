#!/usr/bin/env python3
# Banco de pods simples de Kubernetes (k3s), con la API directa y sin kubectl.
# Mide lo mismo que bench.py: crear en serie y concurrente (hasta Ready), exec echo y memoria.
# Uso: bench_k8s.py  -> imprime una línea JSON.
import base64, json, os, socket, ssl, struct, sys, threading, time, uuid
import urllib.request
from concurrent.futures import ThreadPoolExecutor

SERIE = int(os.environ.get("SERIE", "15"))
CONC = [int(x) for x in os.environ.get("CONC", "10,20").split(",")]
EXECS = int(os.environ.get("EXECS", "30"))
IMAGEN = os.environ.get("IMAGEN", "docker.io/library/alpine:3.20")
NS = "default"

# Credenciales del kubeconfig de k3s (certificado de cliente en línea).
kc = open("/etc/rancher/k3s/k3s.yaml").read()
def campo(n):
    for l in kc.splitlines():
        l = l.strip()
        if l.startswith(n + ":"):
            return base64.b64decode(l.split(":", 1)[1].strip())
d = "/tmp/k8sb"; os.makedirs(d, exist_ok=True)
for n, f in (("certificate-authority-data", "ca"), ("client-certificate-data", "crt"), ("client-key-data", "key")):
    open(f"{d}/{f}", "wb").write(campo(n))
CTX = ssl.create_default_context(cafile=f"{d}/ca")
CTX.load_cert_chain(f"{d}/crt", f"{d}/key")
HOST, PORT = "127.0.0.1", 6443


def api(metodo, ruta, cuerpo=None):
    req = urllib.request.Request(f"https://{HOST}:{PORT}{ruta}", method=metodo,
                                 data=json.dumps(cuerpo).encode() if cuerpo else None,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, context=CTX, timeout=120) as r:
        return json.loads(r.read() or b"{}")


def pct(v, p):
    if not v:
        return None
    v = sorted(v)
    return round(v[min(len(v) - 1, int(round(p / 100 * (len(v) - 1))))], 1)


def resumen(v):
    return {"n": len(v), "p50": pct(v, 50), "p95": pct(v, 95), "max": round(max(v), 1) if v else None}


def mem_usada_mib():
    m = {l.split(":")[0]: int(l.split()[1]) for l in open("/proc/meminfo")}
    return (m["MemTotal"] - m["MemAvailable"]) // 1024


def crear():
    nombre = "b-" + uuid.uuid4().hex[:10]
    t = time.perf_counter()
    api("POST", f"/api/v1/namespaces/{NS}/pods", {
        "apiVersion": "v1", "kind": "Pod", "metadata": {"name": nombre},
        "spec": {"terminationGracePeriodSeconds": 0, "enableServiceLinks": False,
                 "automountServiceAccountToken": False,
                 "containers": [{"name": "c", "image": IMAGEN, "imagePullPolicy": "IfNotPresent",
                                 "command": ["sleep", "infinity"]}]}})
    # Listo = condición Ready del pod, con un watch (sondear a 10 ms desde 20 hilos
    # satura el apiserver y falsea la medida).
    req = urllib.request.Request(
        f"https://{HOST}:{PORT}/api/v1/namespaces/{NS}/pods?watch=1&fieldSelector=metadata.name%3D{nombre}&timeoutSeconds=180")
    with urllib.request.urlopen(req, context=CTX, timeout=200) as r:
        for linea in r:
            p = json.loads(linea)["object"]
            cs = p.get("status", {}).get("conditions", [])
            if any(c["type"] == "Ready" and c["status"] == "True" for c in cs):
                return nombre, (time.perf_counter() - t) * 1000
    raise TimeoutError(nombre)


def borrar(nombre):
    try:
        api("DELETE", f"/api/v1/namespaces/{NS}/pods/{nombre}?gracePeriodSeconds=0")
    except Exception:
        pass


def exec_echo(nombre):
    # Cliente websocket mínimo para /exec (subprotocolo v4.channel.k8s.io).
    ruta = f"/api/v1/namespaces/{NS}/pods/{nombre}/exec?command=echo&command=ack&stdout=true&stderr=true"
    s = CTX.wrap_socket(socket.create_connection((HOST, PORT)), server_hostname=HOST)
    clave = base64.b64encode(os.urandom(16)).decode()
    s.sendall((f"GET {ruta} HTTP/1.1\r\nHost: {HOST}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
               f"Sec-WebSocket-Key: {clave}\r\nSec-WebSocket-Version: 13\r\n"
               f"Sec-WebSocket-Protocol: v4.channel.k8s.io\r\n\r\n").encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    cab, buf = buf.split(b"\r\n\r\n", 1)
    if b" 101 " not in cab.split(b"\r\n")[0]:
        raise RuntimeError(cab[:200])
    salida = b""
    def leer(n):
        nonlocal buf
        while len(buf) < n:
            r = s.recv(65536)
            if not r:
                raise EOFError
            buf += r
        x, buf = buf[:n], buf[n:]
        return x
    while True:
        b0, b1 = leer(2)
        op, ln = b0 & 0x0F, b1 & 0x7F
        if ln == 126:
            ln = struct.unpack(">H", leer(2))[0]
        elif ln == 127:
            ln = struct.unpack(">Q", leer(8))[0]
        datos = leer(ln)
        if op == 8:
            break
        if op in (1, 2) and datos and datos[0] == 1:
            salida += datos[1:]
        if op in (1, 2) and datos and datos[0] == 3:  # canal de estado: fin del comando
            break
    s.close()
    return salida.decode()


res = {"modo": "k8s-pod"}
n, _ = crear(); borrar(n)  # calentamiento (y la imagen ya está en caché)
serie = []
for _ in range(SERIE):
    n, ms = crear(); serie.append(ms); borrar(n)
res["create_serial_ms"] = resumen(serie)
for c in CONC:
    time.sleep(5)
    m0 = mem_usada_mib()
    ok, ts, err = [], [], 0
    with ThreadPoolExecutor(c) as ex:
        for f in [ex.submit(crear) for _ in range(c)]:
            try:
                n, ms = f.result(); ok.append(n); ts.append(ms)
            except Exception as e:
                err += 1; res.setdefault("errores", []).append(str(e)[:120])
    m1 = mem_usada_mib()
    res[f"create_c{c}_ms"] = resumen(ts) | {"fallos": err}
    if ok:
        res[f"mem_por_sandbox_c{c}_mib"] = round((m1 - m0) / len(ok), 1)
    if c == CONC[0] and ok:
        ex_ms = []
        for _ in range(EXECS):
            t = time.perf_counter()
            out = exec_echo(ok[0])
            ex_ms.append((time.perf_counter() - t) * 1000)
            assert out.strip() == "ack", out
        res["exec_echo_ms"] = resumen(ex_ms)
    with ThreadPoolExecutor(c) as ex:
        list(ex.map(borrar, ok))
    # esperar a que desaparezcan antes de la siguiente ronda
    for _ in range(300):
        if not api("GET", f"/api/v1/namespaces/{NS}/pods").get("items"):
            break
        time.sleep(0.2)
print(json.dumps(res))
