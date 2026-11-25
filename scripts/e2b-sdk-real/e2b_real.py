# Prueba el shim `kling sbx e2b` con el SDK real de E2B para Python (paquete de
# PyPI `e2b`, la versión que fija E2B_VERSION). Uso:
#
#   python e2b_real.py <url del shim> <E2B_API_KEY> <directorio> [plantilla]
#   python e2b_real.py --debug <directorio>
#
# Apunta el SDK al shim con E2B_API_URL y E2B_SANDBOX_URL (el modo de pruebas
# locales de docs/e2b.md: control y envd por el mismo listener, el sandbox por
# cabeceras). <directorio> es donde se escriben los ficheros de prueba dentro del
# sandbox (en el test de frontal, un temporal de la máquina; en el lab, /tmp).
#
# Con --debug usa E2B_DEBUG=true: el SDK no llama al plano de control y habla
# con envd en http://localhost:49983 (`kling sbx e2b -envd-listen
# 127.0.0.1:49983 -debug-sandbox <id>`); solo corren los pasos de envd.
# Cada paso imprime OK o FALLA; al final, "E2B OK n/n" o código 1.
import os
import sys
import time
import traceback

E2B_VERSION = "2.64.0"

debug = sys.argv[1] == "--debug"
for k in ("E2B_API_URL", "E2B_SANDBOX_URL", "E2B_DEBUG", "E2B_DOMAIN"):
    os.environ.pop(k, None)
if debug:
    base, plantilla = sys.argv[2].rstrip("/"), "base"
    os.environ.update({"E2B_DEBUG": "true", "E2B_API_KEY": "sin-uso-en-debug"})
else:
    url, clave, base = sys.argv[1], sys.argv[2], sys.argv[3].rstrip("/")
    plantilla = sys.argv[4] if len(sys.argv) > 4 else "base"
    os.environ.update({"E2B_API_URL": url, "E2B_SANDBOX_URL": url, "E2B_API_KEY": clave})

try:
    from importlib.metadata import version as _v
    from e2b import CommandExitException, PtySize, Sandbox, SandboxQuery
except ImportError as e:  # el test de Go lo trata como "saltar"
    print("SIN SDK: %s" % e)
    sys.exit(3)

version = _v("e2b")
if version != E2B_VERSION:
    print("FALLA versión del SDK: %s, se esperaba %s" % (version, E2B_VERSION))
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
    sb = Sandbox.create(plantilla, timeout=300, metadata={"suite": "e2b-real"}, envs={"DE_CREATE": "si"})
    esperar(sb.sandbox_id and "/" not in sb.sandbox_id, "id: %r" % sb.sandbox_id)
    return sb.sandbox_id


def run_ok():
    trozos = []
    r = sb.commands.run("echo hola-e2b", on_stdout=trozos.append)
    esperar(r.exit_code == 0 and r.stdout == "hola-e2b\n", "salida %r exit %r" % (r.stdout, r.exit_code))
    esperar("".join(trozos) == "hola-e2b\n", "on_stdout %r" % trozos)
    return r.stdout


def run_falla():
    try:
        sb.commands.run("echo a-stderr 1>&2; exit 3")
    except CommandExitException as e:
        esperar(e.exit_code == 3 and "a-stderr" in e.stderr, "exit %r stderr %r" % (e.exit_code, e.stderr))
        return e.exit_code
    raise AssertionError("exit 3 no lanzó CommandExitException")


def run_env_cwd():
    r = sb.commands.run('echo "$SALUDO"; pwd', envs={"SALUDO": "hola-env"}, cwd=base)
    esperar(r.stdout.split() in (["hola-env", base], ["hola-env", os.path.realpath(base)]), "salida %r" % r.stdout)
    return r.stdout


def run_env_create():
    # Las envs de Sandbox.create (en debug no hay create: el sandbox ya existe).
    r = sb.commands.run('echo "${DE_CREATE:-no}"')
    esperar(r.stdout == "si\n", "salida %r" % r.stdout)
    return r.stdout


def escribir():
    info = sb.files.write(base + "/hola.txt", "contenido e2b\n")
    esperar(info.path == base + "/hola.txt", "info %r" % info)
    return info.name


def leer():
    t = sb.files.read(base + "/hola.txt")
    esperar(t == "contenido e2b\n", "leído %r" % t)
    b = sb.files.read(base + "/hola.txt", format="bytes")
    esperar(bytes(b) == b"contenido e2b\n", "bytes %r" % b)
    return t


def ficheros():
    esperar(sb.files.exists(base + "/hola.txt"), "exists")
    esperar(not sb.files.exists(base + "/no-existe.txt"), "exists de uno que no existe")
    i = sb.files.get_info(base + "/hola.txt")
    esperar(i.size == 14 and i.type.value == "file", "get_info %r" % i)
    esperar(sb.files.make_dir(base + "/sub"), "make_dir")
    sb.files.rename(base + "/hola.txt", base + "/sub/movido.txt")
    nombres = sorted(e.name for e in sb.files.list(base, depth=2))
    esperar(nombres == ["movido.txt", "sub"], "list %r" % nombres)
    sb.files.remove(base + "/sub")
    esperar(not sb.files.exists(base + "/sub"), "remove")
    return nombres


def write_files():
    sb.files.write_files([{"path": base + "/a.txt", "data": "a"}, {"path": base + "/b.bin", "data": b"\x00\x01"}])
    esperar(sb.files.read(base + "/a.txt") == "a", "a.txt")
    esperar(bytes(sb.files.read(base + "/b.bin", format="bytes")) == b"\x00\x01", "b.bin")
    sb.files.remove(base + "/a.txt")
    sb.files.remove(base + "/b.bin")


def fondo_wait():
    h = sb.commands.run("sleep 0.2; echo de-fondo", background=True)
    esperar(h.pid > 0, "pid %r" % h.pid)
    r = h.wait()
    esperar(r.exit_code == 0 and r.stdout == "de-fondo\n", "wait %r" % r)
    return h.pid


def fondo_kill():
    h = sb.commands.run("sleep 60", background=True)
    pids = [p.pid for p in sb.commands.list()]
    esperar(h.pid in pids, "list %r no tiene %r" % (pids, h.pid))
    esperar(h.kill(), "kill devolvió False")
    try:
        h.wait()
    except CommandExitException as e:
        esperar(e.exit_code != 0, "exit %r" % e.exit_code)
    pids = [p.pid for p in sb.commands.list()]
    esperar(h.pid not in pids, "sigue en list %r" % pids)
    return h.pid


def fondo_connect():
    h = sb.commands.run("sleep 0.5; echo reconectado", background=True)
    h.disconnect()
    h2 = sb.commands.connect(h.pid)
    r = h2.wait()
    esperar("reconectado" in r.stdout, "connect %r" % r)
    return h.pid


def listar():
    p = Sandbox.list(query=SandboxQuery(metadata={"suite": "e2b-real"}))
    ids = [i.sandbox_id for i in p.next_items()]
    esperar(ids == [sb.sandbox_id], "list %r" % ids)
    return ids


def info():
    i = sb.get_info()
    esperar(i.sandbox_id == sb.sandbox_id and i.metadata.get("suite") == "e2b-real", "info %r" % i)
    esperar(sb.is_running(), "is_running")
    return i.state


def timeout():
    sb.set_timeout(600)


def conectar():
    otro = Sandbox.connect(sb.sandbox_id)
    r = otro.commands.run("echo por-connect")
    esperar(r.stdout == "por-connect\n", "salida %r" % r.stdout)
    return r.stdout


def red():
    sb.update_network({"allow_out": ["pypi.org"]})
    sb.update_network({})


def pausar():
    esperar(sb.pause(), "pause devolvió False")
    esperar(sb.get_info().state.value == "paused", "no quedó paused")


def reanudar():
    otro = Sandbox.connect(sb.sandbox_id)
    esperar(otro.get_info().state.value == "running", "no quedó running")
    return otro.commands.run("echo tras-pausa").stdout


def no_soportado():
    # Lo que el gateway no tiene (docs/e2b.md) debe fallar con un error, no colgarse.
    fallos = []
    for nombre, f in [("send_stdin", lambda: sb.commands.send_stdin(1, "x")),
                      ("watch_dir", lambda: sb.files.watch_dir(base)),
                      ("pty.create", lambda: sb.pty.create(size=PtySize(rows=24, cols=80)))]:
        try:
            f()
        except Exception as e:  # noqa: BLE001 - lo esperado
            fallos.append("%s: %s" % (nombre, type(e).__name__))
            continue
        raise AssertionError("%s no falló" % nombre)
    return fallos


def matar():
    sb.kill()
    try:
        sb.get_info()
    except Exception:  # noqa: BLE001 - lo esperado: ya no existe
        return "borrado"
    raise AssertionError("el sandbox sigue existiendo tras kill")


if paso("create", crear):
    paso("commands.run + on_stdout", run_ok)
    paso("commands.run exit 3", run_falla)
    paso("commands.run envs + cwd", run_env_cwd)
    if not debug:
        paso("Sandbox.create(envs) + run", run_env_create)
    if paso("files.write", escribir):
        paso("files.read (text, bytes)", leer)
        paso("files exists/get_info/make_dir/rename/list/remove", ficheros)
    paso("files.write_files", write_files)
    paso("commands.run background + wait", fondo_wait)
    paso("commands.run background + list + kill", fondo_kill)
    paso("commands.connect", fondo_connect)
    paso("send_stdin/watch_dir/pty fallan", no_soportado)
    if debug:
        paso("is_running", sb.is_running)
    else:
        paso("Sandbox.list(metadata)", listar)
        paso("get_info + is_running", info)
        paso("set_timeout", timeout)
        paso("Sandbox.connect + run", conectar)
        paso("update_network", red)
        if paso("pause", pausar):
            paso("Sandbox.connect (resume) + run", reanudar)
        paso("kill", matar)
for p in pasos:
    print("\t".join(p))
ok = sum(1 for p in pasos if p[1] == "OK")
if ok == len(pasos) and pasos:
    print("E2B OK %d/%d (e2b %s%s)" % (ok, len(pasos), version, ", E2B_DEBUG" if debug else ""))
    sys.exit(0)
print("E2B FALLA %d/%d" % (ok, len(pasos)))
sys.exit(1)
