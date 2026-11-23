"""Mide cuántas conexiones abre el SDK y lo que tarda cada llamada (D-09).

No es un test (unittest no lo descubre): se corre a mano.

    python3 sdks/python/tests/bench_conexiones.py [N]

Hace N llamadas a get_sandbox contra el servidor falso por HTTP y, si hay
openssl, por HTTPS con un certificado propio (el cliente lo acepta vía
SSL_CERT_FILE). Da conexiones aceptadas por el servidor y el p50 por llamada.
"""

import os
import shutil
import ssl
import statistics
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from kindling_sandbox import Client  # noqa: E402
from servidor_falso import TOKEN, ServidorFalso  # noqa: E402


def medir(srv, url, n):
    c = Client(url=url, token=TOKEN)
    sb = c.create_sandbox(image="base")
    antes = srv.estado.conexiones
    ds = []
    for _ in range(n):
        ini = time.perf_counter()
        c.get_sandbox(sb.id)
        ds.append(time.perf_counter() - ini)
    return srv.estado.conexiones - antes, statistics.median(ds) * 1e6


def certificado(dir_):
    crt, key = os.path.join(dir_, "c.pem"), os.path.join(dir_, "k.pem")
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1",
                    "-keyout", key, "-out", crt], check=True, capture_output=True)
    return crt, key


def main():
    n = int(sys.argv[1]) if len(sys.argv) > 1 else 500
    with ServidorFalso() as srv:
        con, p50 = medir(srv, srv.url, n)
        print("http  n=%d conexiones=%d p50=%.0f µs" % (n, con, p50))
    if not shutil.which("openssl"):
        print("https: sin openssl, no se mide")
        return
    with tempfile.TemporaryDirectory() as d:
        crt, key = certificado(d)
        os.environ["SSL_CERT_FILE"] = crt
        srv = ServidorFalso()
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(crt, key)
        srv.httpd.socket = ctx.wrap_socket(srv.httpd.socket, server_side=True)
        with srv:
            con, p50 = medir(srv, "https://127.0.0.1:%d" % srv.httpd.server_address[1], n)
            print("https n=%d conexiones=%d p50=%.0f µs" % (n, con, p50))


if __name__ == "__main__":
    main()
