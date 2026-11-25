# Prueba el intérprete de código del shim `kling sbx e2b` con el SDK real
# e2b-code-interpreter (paquete de PyPI `e2b-code-interpreter`, la versión que
# fija CI_VERSION). Uso:
#
#   python e2b_ci.py <url del shim> <E2B_API_KEY> <directorio> [plantilla]
#   python e2b_ci.py --domain <dominio> <E2B_API_KEY> <directorio> [plantilla]
#
# Los dos modos de e2b_real.py (E2B_API_URL + E2B_SANDBOX_URL, o E2B_DOMAIN con
# DNS comodín y TLS; E2B_REAL_PROXY igual). El intérprete va al puerto 49999:
# https://49999-<id>.<dominio> con --domain. La plantilla necesita python3 (y
# bash) en la imagen; el SDK pide `code-interpreter-v1` si no se da.
# Cada paso imprime OK o FALLA; al final, "E2B OK n/n" o código 1.
import os
import sys
import time
import traceback

CI_VERSION = "2.11.10"

for k in ("E2B_API_URL", "E2B_SANDBOX_URL", "E2B_DEBUG", "E2B_DOMAIN"):
    os.environ.pop(k, None)
dominio = sys.argv[1] == "--domain"
if dominio:
    sys.argv.pop(1)
    os.environ["E2B_DOMAIN"] = sys.argv[1]
url, clave, base = sys.argv[1], sys.argv[2], sys.argv[3].rstrip("/")
plantilla = sys.argv[4] if len(sys.argv) > 4 else "code-interpreter-v1"
os.environ["E2B_API_KEY"] = clave
if not dominio:
    os.environ.update({"E2B_API_URL": url, "E2B_SANDBOX_URL": url})
OPTS = {"proxy": os.environ["E2B_REAL_PROXY"]} if os.environ.get("E2B_REAL_PROXY") else {}

try:
    from importlib.metadata import version as _v
    from e2b_code_interpreter import Sandbox
except ImportError as e:  # el test de Go lo trata como "saltar"
    print("SIN SDK: %s" % e)
    sys.exit(3)

version = _v("e2b-code-interpreter")
if version != CI_VERSION:
    print("FALLA versión del SDK: %s, se esperaba %s" % (version, CI_VERSION))
    sys.exit(1)

pasos = []


def paso(nombre, f):
    t0 = time.monotonic()
    try:
        r = f()
        pasos.append((nombre, "OK", "%.0f ms" % ((time.monotonic() - t0) * 1000), repr(r)[:100]))
    except Exception as e:  # noqa: BLE001 - se informa y se sigue
        pasos.append((nombre, "FALLA", "", (type(e).__name__ + ": " + str(e))[:300]))
        traceback.print_exc()
    return pasos[-1][1] == "OK"


def esperar(cond, msg):
    if not cond:
        raise AssertionError(msg)


sb = None


def crear():
    global sb
    sb = Sandbox.create(plantilla, timeout=300, **OPTS)
    return sb.sandbox_id


def resultado():
    e = sb.run_code("x = 21\nx * 2")
    esperar(e.error is None and e.text == "42", "text %r error %r" % (e.text, e.error))
    esperar(e.execution_count and e.execution_count >= 1, "execution_count %r" % e.execution_count)
    return e.text


def estado():
    # Sin contexto, run_code usa el de por defecto: las variables siguen.
    e = sb.run_code("x")
    esperar(e.text == "21", "text %r" % e.text)
    return e.text


def salida():
    trozos = []
    e = sb.run_code("print('hola-ci')\nimport sys; print('aviso', file=sys.stderr)", on_stdout=lambda m: trozos.append(m.line))
    esperar("".join(e.logs.stdout) == "hola-ci\n" and "aviso" in "".join(e.logs.stderr), "logs %r" % e.logs)
    esperar("".join(trozos) == "hola-ci\n", "on_stdout %r" % trozos)
    return e.logs.stdout


def error():
    e = sb.run_code("1/0")
    esperar(e.error is not None and e.error.name == "ZeroDivisionError" and "division" in e.error.value, "error %r" % e.error)
    esperar("ZeroDivisionError" in e.error.traceback, "traceback %r" % e.error.traceback)
    return e.error.name


def html():
    e = sb.run_code("class H:\n    def _repr_html_(self):\n        return '<b>hola</b>'\nH()")
    esperar(e.results and e.results[0].html == "<b>hola</b>", "results %r" % e.results)
    return e.results[0].formats()


def bash():
    e = sb.run_code("echo de-bash", language="bash")
    esperar("".join(e.logs.stdout) == "de-bash\n", "logs %r" % e.logs)
    return e.logs.stdout


def contextos():
    c = sb.create_code_context(cwd=base)
    esperar(c.id and c.language == "python", "contexto %r" % c)
    e = sb.run_code("import os; os.getcwd()", context=c)
    esperar(e.text in (repr(base), repr(os.path.realpath(base))), "cwd %r" % e.text)
    e = sb.run_code("x", context=c)
    esperar(e.error is not None and e.error.name == "NameError", "el contexto nuevo ve x: %r" % e.error)
    ids = [x.id for x in sb.list_code_contexts()]
    esperar(c.id in ids, "list %r" % ids)
    sb.remove_code_context(c)
    esperar(c.id not in [x.id for x in sb.list_code_contexts()], "sigue tras remove")
    return c.id


def no_soportado():
    # Lo que el gateway no tiene (docs/e2b.md) debe fallar con un error claro.
    fallos = []
    c = sb.create_code_context()
    for nombre, f in [("run_code(envs=)", lambda: sb.run_code("1", envs={"A": "1"})),
                      ("restart_code_context", lambda: sb.restart_code_context(c))]:
        try:
            f()
        except Exception as e:  # noqa: BLE001 - lo esperado
            fallos.append("%s: %s" % (nombre, type(e).__name__))
            continue
        raise AssertionError("%s no falló" % nombre)
    sb.remove_code_context(c)
    return fallos


def matar():
    sb.kill()


if paso("create", crear):
    paso("run_code -> result text", resultado)
    paso("run_code (estado del contexto por defecto)", estado)
    paso("run_code stdout/stderr + on_stdout", salida)
    paso("run_code error", error)
    paso("run_code result html", html)
    paso("run_code language=bash", bash)
    paso("create/list/remove_code_context + run_code(context=)", contextos)
    paso("envs/restart fallan", no_soportado)
    paso("kill", matar)
for p in pasos:
    print("\t".join(p))
ok = sum(1 for p in pasos if p[1] == "OK")
if ok == len(pasos) and pasos:
    print("E2B OK %d/%d (e2b-code-interpreter %s%s)" % (ok, len(pasos), version, ", E2B_DOMAIN" if dominio else ""))
    sys.exit(0)
print("E2B FALLA %d/%d" % (ok, len(pasos)))
sys.exit(1)
