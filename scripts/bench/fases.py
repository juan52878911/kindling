#!/usr/bin/env python3
"""Tabla por fase a partir de /metrics del daemon y del gateway, antes y después de una tanda.

Uso como biblioteca (lo hace bench.py):
    f = fases.fuentes(url_gateway, token, socket_daemon)
    antes = fases.leer(f); ...tanda...; tabla = fases.tabla(antes, fases.leer(f), latencias_cliente_ms)

Uso suelto (para repetir a mano):
    fases.py snap  > antes.json           # lee las dos fuentes
    fases.py diff antes.json despues.json # imprime la tabla (JSON)

Entorno: KB_URL (gateway, http://127.0.0.1:18099), KTOK (token del inquilino),
KB_DAEMON_SOCK (/run/kt-bench/kling.sock). Solo librería estándar.
Los p50/p95 salen de los cubos acumulados (interpolación lineal dentro del cubo): son
una cota con la resolución de los cubos, no un percentil exacto. Los contadores
`*_ms_total{phase=...}` (p. ej. kling_wake_phase_ms_total) se dividen por el delta de
`kling_operation_duration_ms_count{kind=...}` para dar la media por fase.
"""
import http.client
import json
import os
import re
import socket
import sys
import urllib.request

_LINEA = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)')
_ETQ = re.compile(r'(\w+)="((?:[^"\\]|\\.)*)"')


class _Unix(http.client.HTTPConnection):
    def __init__(self, ruta, timeout=10):
        super().__init__("localhost", timeout=timeout)
        self._ruta = ruta

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self._ruta)


def fuentes(url, token, sock):
    """Devuelve {nombre: función que da el texto de /metrics}. Sin token no se lee el gateway."""
    def gw():
        rq = urllib.request.Request(url.rstrip("/") + "/v1/metrics", headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(rq, timeout=10) as r:
            return r.read().decode()

    def dm():
        c = _Unix(sock)
        try:
            c.request("GET", "/metrics")
            return c.getresponse().read().decode()
        finally:
            c.close()
    out = {"daemon": dm}
    if token:
        out["gateway"] = gw
    return out


def parse(texto):
    """Texto Prometheus -> {(nombre, ((k, v), ...)): valor}."""
    m = {}
    for l in texto.splitlines():
        if l.startswith("#"):
            continue
        g = _LINEA.match(l)
        if not g:
            continue
        try:
            v = float(g.group(3))
        except ValueError:
            continue
        etq = tuple(sorted(_ETQ.findall(g.group(2) or "")))
        m[(g.group(1), etq)] = v
    return m


def leer(fs):
    """Lee todas las fuentes; una que falle queda vacía (el banco no se cae por la telemetría)."""
    out = {}
    for n, f in fs.items():
        try:
            out[n] = parse(f())
        except Exception as e:  # noqa: BLE001 - cualquier fallo de red o formato
            print(f"fases: {n} sin /metrics: {e}", file=sys.stderr)
            out[n] = {}
    return out


def _cuantil(cubos, total, q):
    """cubos: [(le, acumulado)] ordenados; interpolación lineal dentro del cubo."""
    if total <= 0:
        return None
    objetivo = q * total
    ant_le, ant_c = 0.0, 0.0
    for le, c in cubos:
        if c >= objetivo:
            if le == float("inf"):
                return ant_le
            if c == ant_c:
                return le
            return round(ant_le + (le - ant_le) * (objetivo - ant_c) / (c - ant_c), 1)
        ant_le, ant_c = le, c
    return ant_le


def _pct(v, p):
    if not v:
        return None
    v = sorted(v)
    return round(v[min(len(v) - 1, int(round(p / 100 * (len(v) - 1))))], 1)


def tabla(antes, despues, cliente_ms=None):
    """Una fila por histograma (p50/p95/media desde los cubos) y por contador de fase
    (media por operación), con el delta entre las dos lecturas, más la fila del cliente."""
    filas = []
    if cliente_ms:
        filas.append({"fuente": "cliente", "fase": "crear (extremo a extremo)", "n": len(cliente_ms),
                      "p50_ms": _pct(cliente_ms, 50), "p95_ms": _pct(cliente_ms, 95),
                      "media_ms": round(sum(cliente_ms) / len(cliente_ms), 1)})
    for fuente in sorted(despues):
        d, a = despues[fuente], antes.get(fuente, {})

        def delta(k):
            return d.get(k, 0.0) - a.get(k, 0.0)
        # Histogramas: agrupa los *_bucket por (nombre base, etiquetas sin "le").
        grupos = {}
        for (nom, etq), _ in d.items():
            if nom.endswith("_bucket"):
                le = dict(etq).get("le")
                if le is None:
                    continue
                resto = tuple(e for e in etq if e[0] != "le")
                grupos.setdefault((nom[:-7], resto), []).append(
                    (float("inf") if le == "+Inf" else float(le), delta((nom, etq))))
        for (base, etq), cubos in sorted(grupos.items()):
            cubos.sort()
            n = delta((base + "_count", etq))
            if n <= 0:
                continue
            s = delta((base + "_sum", etq))
            filas.append({"fuente": fuente, "fase": base + "".join(f"[{k}={v}]" for k, v in etq), "n": int(n),
                          "p50_ms": _cuantil(cubos, n, 0.50), "p95_ms": _cuantil(cubos, n, 0.95),
                          "media_ms": round(s / n, 1)})
        # Contadores de fase: media por operación de su clase.
        for (nom, etq), _ in sorted(d.items()):
            if not nom.endswith("_ms_total") or "phase" not in dict(etq):
                continue
            kind = dict(etq).get("kind")
            n = delta(("kling_operation_duration_ms_count", (("kind", kind),))) if kind else 0
            dv = delta((nom, etq))
            if n > 0 and dv > 0:
                filas.append({"fuente": fuente, "fase": f"{nom[:-6]}[{kind}:{dict(etq)['phase']}]", "n": int(n),
                              "p50_ms": None, "p95_ms": None, "media_ms": round(dv / n, 1)})
    return filas


def _codificar(snap):
    return {f: [[n, list(map(list, e)), v] for (n, e), v in m.items()] for f, m in snap.items()}


def _decodificar(j):
    return {f: {(n, tuple(map(tuple, e))): v for n, e, v in filas} for f, filas in j.items()}


def _main(argv):
    if argv[:1] == ["snap"]:
        fs = fuentes(os.environ.get("KB_URL", "http://127.0.0.1:18099"), os.environ.get("KTOK", ""),
                     os.environ.get("KB_DAEMON_SOCK", "/run/kt-bench/kling.sock"))
        print(json.dumps(_codificar(leer(fs))))
        return 0
    if argv[:1] == ["diff"] and len(argv) == 3:
        a, b = (_decodificar(json.load(open(p))) for p in argv[1:])
        print(json.dumps(tabla(a, b), indent=1))
        return 0
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(_main(sys.argv[1:]))
