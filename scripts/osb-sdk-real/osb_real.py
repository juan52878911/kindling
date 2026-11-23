# Prueba el shim `kling sbx osb` con el SDK real de OpenSandbox (paquete de PyPI
# `opensandbox`, la versión que fija OSB_VERSION). Uso:
#
#   python osb_real.py <url del shim> <OPEN-SANDBOX-API-KEY> <directorio> [imagen]
#
# <directorio> es donde se escriben los ficheros de prueba dentro del sandbox
# (en el test de frontal, un temporal de la máquina; en el lab, por ejemplo /tmp).
# Cada paso imprime OK o FALLA; al final, "OSB OK n/n" o código 1.
import sys
import time
import traceback
from datetime import datetime, timedelta, timezone

OSB_VERSION = "1.2.1"

try:
    import opensandbox
    from opensandbox.config.connection_sync import ConnectionConfigSync
    from opensandbox.sync.sandbox import SandboxSync
    from opensandbox.models.execd import RunCommandOpts
    from opensandbox.models.filesystem import (ContentReplaceEntry, DirectoryListEntry, MoveEntry,
                                               SearchEntry, WriteEntry)
    from opensandbox.models.sandboxes import NetworkRule, SandboxFilter
    from opensandbox.sync.manager import SandboxManagerSync
except ImportError as e:  # el test de Go lo trata como "saltar"
    print("SIN SDK: %s" % e)
    sys.exit(3)

url, clave, base = sys.argv[1], sys.argv[2], sys.argv[3].rstrip("/")
imagen = sys.argv[4] if len(sys.argv) > 4 else "python:3.12"
version = getattr(opensandbox, "__version__", None)
if version is None:
    from importlib.metadata import version as _v
    version = _v("opensandbox")
if version != OSB_VERSION:
    print("FALLA versión del SDK: %s, se esperaba %s" % (version, OSB_VERSION))
    sys.exit(1)

cfg = ConnectionConfigSync(domain=url, api_key=clave, request_timeout=timedelta(seconds=30))
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
    sb = SandboxSync.create(imagen, connection_config=cfg, timeout=timedelta(minutes=5),
                            resource={"cpu": "1", "memory": "512Mi"}, metadata={"suite": "osb-real"},
                            ready_timeout=timedelta(seconds=30))
    esperar(sb.id and "/" not in sb.id, "id: %r" % sb.id)
    return sb.id


def run_ok():
    r = sb.commands.run("echo hola-osb")
    out = "".join(m.text for m in r.logs.stdout)
    esperar(r.error is None and out.strip() == "hola-osb", "salida %r error %r" % (out, r.error))
    return out.strip()


def run_falla():
    r = sb.commands.run("echo a-stderr 1>&2; exit 3")
    err = "".join(m.text for m in r.logs.stderr)
    esperar(r.error is not None and r.exit_code == 3 and "a-stderr" in err, "exit %r error %r stderr %r" % (r.exit_code, r.error, err))
    return r.exit_code


def escribir():
    sb.files.write_files([WriteEntry(path=base + "/hola.txt", data="contenido osb\n", mode=644)])


def leer():
    t = sb.files.read_file(base + "/hola.txt")
    esperar(t == "contenido osb\n", "leído %r" % t)
    return t


def ficheros():
    i = sb.files.get_file_info([base + "/hola.txt"])[base + "/hola.txt"]
    esperar(i.size == 14 and i.mode == 644, "info %r" % i)
    sb.files.replace_contents([ContentReplaceEntry(path=base + "/hola.txt", old_content="osb", new_content="kindling")])
    sb.files.move_files([MoveEntry(src=base + "/hola.txt", dest=base + "/movido.txt")])
    esperar(sb.files.read_file(base + "/movido.txt") == "contenido kindling\n", "replace + mv")
    halla = [e.path for e in sb.files.search(SearchEntry(path=base, pattern="*.txt"))]
    esperar(any(p.endswith("/movido.txt") for p in halla), "search %r" % halla)
    lista = [e.path for e in sb.files.list_directory(DirectoryListEntry(path=base, depth=1))]
    esperar(any(p.endswith("/movido.txt") for p in lista), "list %r" % lista)
    sb.files.delete_files([base + "/movido.txt"])
    return len(halla)


def fondo():
    r = sb.commands.run("echo de-fondo", opts=RunCommandOpts(background=True))
    esperar(r.id, "sin id")
    for _ in range(100):
        st = sb.commands.get_command_status(r.id)
        if not st.running:
            break
        time.sleep(0.05)
    esperar(st.exit_code == 0, "estado %r" % st)
    logs = sb.commands.get_background_command_logs(r.id)
    esperar("de-fondo" in logs.content, "logs %r" % logs)
    return r.id


def sesion():
    sid = sb.commands.create_session(working_directory=base)
    r = sb.commands.run_in_session(sid, "echo en-sesion")
    out = "".join(m.text for m in r.logs.stdout)
    esperar(out.strip() == "en-sesion", "salida %r" % out)
    return sid


def listar():
    m = SandboxManagerSync.create(connection_config=cfg)
    p = m.list_sandbox_infos(SandboxFilter(metadata={"suite": "osb-real"}, page_size=10))
    esperar(len(p.sandbox_infos) == 1 and p.sandbox_infos[0].id == sb.id, "list %r" % p)
    return p.pagination.total_items


def red():
    sb.patch_egress_rules([NetworkRule(action="allow", target="pypi.org")])
    pol = sb.get_egress_policy()
    esperar([r.target for r in pol.egress or []] == ["pypi.org"], "política %r" % pol)
    return pol.default_action


def info():
    i = sb.get_info()
    esperar(i.status.state == "Running", "estado %r" % i.status.state)
    esperar(i.metadata.get("suite") == "osb-real", "metadata %r" % i.metadata)
    return i.status.state


def renovar():
    return sb.renew(timedelta(minutes=10))


def pausar():
    sb.pause()
    esperar(sb.get_info().status.state == "Paused", "no quedó Paused")


def reanudar():
    global sb
    sb = SandboxSync.resume(sb.id, connection_config=cfg)
    esperar(sb.get_info().status.state == "Running", "no quedó Running")
    return run_ok()


def matar():
    sb.kill()
    try:
        sb.get_info()
    except Exception:  # noqa: BLE001 - lo esperado: ya no existe
        return "borrado"
    raise AssertionError("el sandbox sigue existiendo tras kill")


if paso("create", crear):
    paso("commands.run", run_ok)
    paso("commands.run exit 3", run_falla)
    if paso("files.write_files", escribir):
        paso("files.read_file", leer)
        paso("files info/replace/mv/search/list/delete", ficheros)
    paso("commands.run background + status + logs", fondo)
    paso("create_session + run_in_session", sesion)
    paso("list_sandbox_infos", listar)
    paso("patch_egress_rules + get_egress_policy", red)
    paso("get_info", info)
    paso("renew", renovar)
    if paso("pause", pausar):
        paso("resume + run", reanudar)
    paso("kill", matar)
for p in pasos:
    print("\t".join(p))
ok = sum(1 for p in pasos if p[1] == "OK")
if ok == len(pasos) and pasos:
    print("OSB OK %d/%d (opensandbox %s)" % (ok, len(pasos), version))
    sys.exit(0)
print("OSB FALLA %d/%d" % (ok, len(pasos)))
sys.exit(1)
