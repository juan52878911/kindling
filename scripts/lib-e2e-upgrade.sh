# shellcheck shell=bash
# lib-e2e-upgrade.sh — lo que comparten 94-e2e-upgrade.sh (Linux) y
# 95-e2e-upgrade-mac.sh (macOS): elegir la release anterior, leer esquemas y
# servir una release por https en local. Se carga con `.`; bash 3.2 (macOS):
# nada de arrays asociativos.

# ultima_release_anterior NUEVA imprime la etiqueta de la última release
# publicada (ni borrador ni prerelease) que no es NUEVA y no es más nueva por
# sus tres números: con NUEVA = 0.21.0-e2e.abc o 0.21.0-rc.1 es la 0.20.x;
# con NUEVA = 0.20.0 (el commit etiquetado), la anterior a ella. Es la N-1
# que promete docs/actualizar.md. Usa el API de GitHub (GITHUB_TOKEN, si lo
# hay, para no chocar con su límite) y vacío si no puede preguntar.
ultima_release_anterior() {
  local repo="${KLING_E2E_REPO:-juan52878911/kindling}"
  # shellcheck disable=SC2086 # la cabecera va entera o no va
  curl -fsSL --max-time 30 ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
    "https://api.github.com/repos/$repo/releases?per_page=50" 2>/dev/null \
  | python3 -c '
import json, re, sys
def num(t):
    m = re.match(r"v?(\d+)\.(\d+)\.(\d+)", t)
    return tuple(int(x) for x in m.groups()) if m else None
nueva = sys.argv[1].lstrip("v")
n = num(nueva)
mejor = None
try:
    rels = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for r in rels if isinstance(rels, list) else []:
    t = r.get("tag_name", "")
    v = num(t)
    if r.get("draft") or r.get("prerelease") or v is None or n is None:
        continue
    if t.lstrip("v") == nueva or v > n:
        continue
    if mejor is None or v > mejor[0]:
        mejor = (v, t)
print(mejor[1] if mejor else "")
' "$1"
}

# schema_de FICHERO: el "schema" de un state.json (0 si es una lista, el
# formato de antes de v0.18; vacío si no se puede leer).
schema_de() {
  python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('schema',0) if isinstance(d,dict) else 0)" "$1" 2>/dev/null
}

# esquema_soportado BIN CLAVE: lo que `BIN upgrade -schemas` dice que lee de
# CLAVE (state, meta, credentials).
esquema_soportado() {
  "$1" upgrade -schemas 2>/dev/null | python3 -c "import json,sys; print(json.load(sys.stdin)['schemas'][sys.argv[1]])" "$2" 2>/dev/null
}

# mismo_estado compara dos state.json por su contenido: v0.17 los escribe
# desde un mapa, en otro orden cada vez.
mismo_estado() {
  python3 - "$1" "$2" <<'PY'
import json, sys
def norma(x):
    if isinstance(x, dict):
        return {k: norma(v) for k, v in x.items()}
    if isinstance(x, list):
        x = [norma(v) for v in x]
        if all(isinstance(v, dict) and "id" in v for v in x):
            x.sort(key=lambda v: v["id"])
        return x
    return x
try:
    a, b = (norma(json.load(open(p))) for p in sys.argv[1:3])
except (OSError, ValueError):
    sys.exit(1)
sys.exit(0 if a == b else 1)
PY
}

# etiqueta_valida: lo que `kling upgrade -tag` acepta (internal/upgrade:
# reEtiqueta). Un `git describe` con commits encima (v0.20.0-3-gabc) no lo es.
etiqueta_valida() {
  printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
}

# servir_release WWW ETIQUETA DIR arranca un servidor https en 127.0.0.1 con
# un certificado propio (en DIR) que imita a GitHub: /releases/latest redirige
# a /releases/tag/ETIQUETA y /releases/download/... sirve los ficheros de WWW.
# Deja SRV_PID y SRV_URL. Para que kling se fíe del certificado,
# SSL_CERT_FILE=DIR/cert.pem (Go lo respeta en Linux; en macOS no).
servir_release() {
  local www="$1" tag="$2" dir="$3" port=""
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=127.0.0.1 \
    -addext "subjectAltName=IP:127.0.0.1" \
    -keyout "$dir/key.pem" -out "$dir/cert.pem" >/dev/null 2>&1 || return 1
  python3 - "$www" "$tag" "$dir" >"$dir/server.log" 2>&1 <<'PY' &
import functools, http.server, os, ssl, sys
www, tag, d = sys.argv[1:4]
class H(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path.rstrip("/") == "/releases/latest":
            self.send_response(302)
            self.send_header("Location", "/releases/tag/" + tag)
            self.end_headers()
            return
        return super().do_GET()
srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(H, directory=www))
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(d + "/cert.pem", d + "/key.pem")
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
with open(d + "/port.tmp", "w") as f:
    f.write(str(srv.server_address[1]))
os.rename(d + "/port.tmp", d + "/port")
srv.serve_forever()
PY
  # shellcheck disable=SC2034 # SRV_PID y SRV_URL son para quien llama
  SRV_PID=$!
  for _ in $(seq 1 50); do
    [ -s "$dir/port" ] && { port=$(cat "$dir/port"); break; }
    sleep 0.1
  done
  [ -n "$port" ] || return 1
  # shellcheck disable=SC2034
  SRV_URL="https://127.0.0.1:$port"
}
