"""Recorre el SDK de Python contra el frontal real (lo lanza TestContratoSDKs, en Go).

Uso: contrato.py URL TOKEN BASE DOMINIO. BASE es un directorio de esta máquina donde el
invitado de prueba (un daemon que ejecuta de verdad) deja ficheros. Imprime "CONTRATO OK"
al final; cualquier fallo termina con código 1 diciendo en qué paso.
"""

import io
import os
import sys
import tarfile

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from kindling_sandbox import ApiError, Client, Conflict, NotFound, Sandbox  # noqa: E402

url, token, base, dominio = sys.argv[1:5]
paso = "inicio"


def p(nombre):
    global paso
    paso = nombre
    print("paso:", nombre, flush=True)


def igual(real, esperado):
    assert real == esperado, "%s: %r != %r" % (paso, real, esperado)


try:
    kw = dict(url=url, token=token)
    p("create con env, labels y ttl")
    sb = Sandbox.create(image="base", env={"A": "1"}, labels={"suite": "contrato"}, ttl=600, **kw)
    otro = Sandbox.create(image="base", labels={"suite": "otro"}, **kw)
    igual(sb.info.state, "running")

    p("list: filtros, limit y cursor")
    igual(sorted(s.id for s in Sandbox.list(label={"suite": "contrato"}, **kw)), [sb.id])
    igual(len(Sandbox.list(state="running", **kw)), 2)
    pag = Sandbox.list(limit=1, **kw)
    igual(len(pag), 1)
    assert pag.next_cursor, "sin cursor de la página siguiente"
    sig = Sandbox.list(limit=1, cursor=pag.next_cursor, **kw)
    igual(len(sig), 1)
    assert sig[0].id != pag[0].id

    p("connect, renew, pause, resume, set_egress")
    igual(Sandbox.connect(sb.id, **kw).id, sb.id)
    sb.renew(900)
    igual(sb.pause().state, "paused")
    igual(sb.resume().state, "running")
    igual(sb.set_egress("allowlist", ["a.example.test"]).applied, "live")

    p("endpoints: create, list, revoke")
    ep = sb.endpoints.create(3000, ttl=300, public=True)
    igual((ep.port, ep.public), (3000, True))
    assert dominio in ep.url, ep.url
    igual([e.port for e in sb.endpoints.list()], [3000])
    sb.endpoints.revoke(3000)
    igual(sb.endpoints.list(), [])

    p("exec")
    r = sb.exec("echo hola; exit 3")
    igual((r.exit_code, r.stdout), (3, "hola\n"))

    p("ficheros: write, read, range, stat")
    f = os.path.join(base, "a.log")
    sb.files.write(f, "0123456789")
    sb.files.write(os.path.join(base, "b.txt"), "x")
    igual(sb.files.read(f), b"0123456789")
    igual(sb.files.read(f, range=(2, 5)), b"2345")
    igual(sb.files.read(f, range="bytes=-3"), b"789")
    try:
        sb.files.read(f, range=(50, None))
        raise AssertionError("un Range fuera del fichero tenía que dar 416")
    except ApiError as e:
        igual((e.status, e.code), (416, "requested_range_not_satisfiable"))
    igual(sb.files.stat(f).size, 10)

    p("ficheros: search y move")
    igual(sb.files.search(base, "*.log").matches, [f])
    destino = os.path.join(base, "c.txt")
    sb.files.write(destino, "ya existe")
    try:
        sb.files.move(os.path.join(base, "b.txt"), destino)
        raise AssertionError("move sobre un destino que existe tenía que dar 409")
    except Conflict:
        pass
    sb.files.move(os.path.join(base, "b.txt"), destino, overwrite=True)
    igual(sb.files.read(destino), b"x")
    try:
        sb.files.search(os.path.join(base, "no-existe"), "*")
        raise AssertionError("search en una raíz inexistente tenía que dar 404")
    except NotFound:
        pass

    p("ficheros: mkdir y tar")
    assert sb.files.mkdir("/arbol/x").is_dir
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tf:
        ti = tarfile.TarInfo("a.txt")
        ti.size = 2
        tf.addfile(ti, io.BytesIO(b"hi"))
    igual(sb.files.upload_tar("/paquete", buf.getvalue(), mkdir=True).entries, 1)
    with tarfile.open(fileobj=io.BytesIO(sb.files.download_tar("/paquete"))) as tf:
        igual(tf.extractfile("a.txt").read(), b"hi")

    p("ficheros: chmod y replace")
    conf = os.path.join(base, "app.conf")
    sb.files.write(conf, "host=a\nport=1\nport=1\n")
    sb.files.chmod(conf, 0o600)
    igual(oct(os.stat(conf).st_mode & 0o777), "0o600")
    igual(sb.files.replace(conf, "host=a", "host=b").replacements, 1)
    try:
        sb.files.replace(conf, "port=1", "port=2")
        raise AssertionError("un old repetido sin all tenía que dar 409")
    except Conflict:
        pass
    igual(sb.files.replace(conf, "port=1", "port=2", all=True).replacements, 2)
    igual(sb.files.read_text(conf), "host=b\nport=2\nport=2\n")

    p("metadata, metrics y fork")
    igual(sb.set_metadata({"fase": "dos"}, remove=["suite"]).labels, {"fase": "dos"})
    igual(Sandbox.connect(sb.id, **kw).info.labels, {"fase": "dos"})
    try:
        sb.set_metadata({"tenant": "otro"})
        raise AssertionError("una etiqueta reservada tenía que dar 400")
    except ApiError as e:
        igual(e.status, 400)
    igual(sb.metrics().id, sb.id)
    copias = sb.fork(count=2, ttl=120)
    igual(len(copias), 2)
    igual(copias[0].info.labels.get("fase"), "dos")
    for c in copias:
        c.delete()

    p("code: contextos, run, run_wait, interrupt")
    ctx = sb.code.contexts.create(language="python")
    igual([c.id for c in sb.code.contexts.list()], [ctx.id])
    igual(sb.code.contexts.get(ctx.id).language, "python")
    evs = list(sb.code.run("x = 21", context=ctx.id))
    igual(evs[-1].type, "done")
    igual(sb.code.run_wait("x * 2", context=ctx.id).results["text/plain"], "42")
    mal = sb.code.run_wait("1/0", context=ctx.id)
    igual((mal.ok, mal.error.ename), (False, "ZeroDivisionError"))
    igual(sb.code.run_wait("print(1 + 2)").stdout, "3\n")  # contexto efímero
    sb.code.interrupt(ctx.id)
    sb.code.contexts.delete(ctx.id)
    try:
        sb.code.contexts.get(ctx.id)
        raise AssertionError("el contexto borrado tenía que dar 404")
    except NotFound:
        pass

    p("errores con code")
    try:
        Sandbox.connect("a/no-existe", **kw)
        raise AssertionError("tenía que dar 404")
    except NotFound as e:
        igual(e.code, "not_found")
    try:
        Client(url=url, token="mal").hosts()
        raise AssertionError("tenía que dar 401")
    except ApiError as e:
        igual((e.status, e.code), (401, "unauthorized"))

    p("delete")
    sb.delete()
    otro.delete()
    igual(Sandbox.list(**kw), [])
    print("CONTRATO OK")
except AssertionError as e:
    print("FALLO en el paso %r: %s" % (paso, e))
    sys.exit(1)
except Exception as e:  # noqa: BLE001
    print("ERROR en el paso %r: %s: %s" % (paso, type(e).__name__, e))
    sys.exit(1)
