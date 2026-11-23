import asyncio
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from kindling_sandbox import (Admin, AsyncSandbox, Forbidden, Client, ConfigError, NotFound, RateLimited, Sandbox,
                              TransportError, Unauthorized, ApiError, BadRequest, Conflict, ServerError)

from servidor_falso import TOKEN, TOKEN_USUARIO, ServidorFalso


class Base(unittest.TestCase):
    def setUp(self):
        self.srv = ServidorFalso()
        self.srv.__enter__()
        self.addCleanup(self.srv.__exit__)
        self.kw = dict(url=self.srv.url, token=TOKEN)


class TestProcesosYSesiones(Base):
    def setUp(self):
        super().setUp()
        self.sb = Sandbox.create(**self.kw)

    def test_start_manda_argv_y_devuelve_pid(self):
        p = self.sb.processes.start("sleep 9", cwd="/w", env={"A": "1"}, timeout=30, session="dev")
        self.assertEqual((p.pid, p.state, p.running), (1, "running", True))
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]),
                         {"cmd": ["sh", "-c", "sleep 9"], "dir": "/w", "env": ["A=1"],
                          "timeout_seconds": 30, "session": "dev"})

    def test_list_get_con_wait_y_delete(self):
        p = self.sb.processes.start(["sleep", "9"])
        self.assertEqual([x.pid for x in self.sb.processes.list()], [p.pid])
        fin = self.sb.processes.get(p.pid, wait=5)
        self.assertEqual((fin.state, fin.exit_code), ("exited", 0))
        self.assertEqual(self.srv.estado.peticiones[-1][2], {"wait": "5"})
        self.sb.processes.delete(p.pid)
        with self.assertRaises(NotFound):
            self.sb.processes.get(p.pid)

    def test_signal_y_kill(self):
        p = self.sb.processes.start(["x"])
        self.assertEqual(self.sb.processes.signal(p.pid).signal, "TERM")
        self.assertEqual(self.sb.processes.kill(p.pid).signal, "KILL")
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]), {"signal": "KILL"})

    def test_output_con_offset_y_follow(self):
        p = self.sb.processes.start(["x"])
        evs = list(self.sb.processes.output(p.pid, follow=True))
        self.assertEqual(b"".join(e.data for e in evs), b"hola\nmundo\n")
        self.assertEqual((evs[-1].exit, evs[-1].offset), (0, 11))
        resto = list(self.sb.processes.output(p.pid, offset=5))
        self.assertEqual(b"".join(e.data for e in resto), b"mundo\n")
        self.assertIsNone(resto[-1].exit)  # sin follow no hay exit

    def test_sesiones_crear_exec_borrar(self):
        se = self.sb.sessions.create("dev", cwd="/w", env={"A": "1"}, ttl=60)
        self.assertEqual((se.id, se.dir, se.env), ("dev", "/w", ["A=1"]))
        r = self.sb.sessions.exec("dev", "pwd")
        self.assertEqual(r.exit_code, 3)
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4])["session"], "dev")
        self.assertEqual([s.id for s in self.sb.sessions.list()], ["dev"])
        self.sb.sessions.delete("dev")
        with self.assertRaises(NotFound):
            self.sb.sessions.get("dev")

    def test_exec_con_session(self):
        self.sb.exec("pwd", session="dev")
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4])["session"], "dev")

    def test_asincrono(self):
        async def corre():
            async with await AsyncSandbox.create(**self.kw) as sb:
                p = await sb.processes.start(["x"])
                it = await sb.processes.output(p.pid, follow=True)
                evs = [e async for e in it]
                se = await sb.sessions.create("a")
                return evs, se
        evs, se = asyncio.run(corre())
        self.assertEqual(evs[-1].exit, 0)
        self.assertEqual(se.id, "a")


class TestLoteYVolumenes(Base):
    def test_create_many_manda_count_y_atomic(self):
        lote = Sandbox.create_many(3, template="py", atomic=True, **self.kw)
        self.assertEqual([s.id for s in lote], ["h1/m-1", "h1/m-2", "h1/m-3"])
        self.assertEqual(lote.errors, [])
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]),
                         {"template": "py", "count": 3, "atomic": True})

    def test_lote_parcial_trae_errores(self):
        self.srv.estado.lote_falla_desde = 2
        lote = Sandbox.create_many(4, **self.kw)
        self.assertEqual(len(lote), 2)
        self.assertEqual([e["index"] for e in lote.errors], [2, 3])

    def test_lote_atomico_que_falla_lanza_y_no_deja_nada(self):
        self.srv.estado.lote_falla_desde = 1
        with self.assertRaises(ApiError):
            Sandbox.create_many(3, atomic=True, **self.kw)
        self.assertEqual(self.srv.estado.sandboxes, {})

    def test_create_con_volumenes(self):
        Sandbox.create(volumes=[{"name": "datos", "mount": "/data", "read_only": True}], **self.kw)
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]),
                         {"volumes": [{"name": "datos", "mount": "/data", "read_only": True}]})

    def test_crud_de_volumenes(self):
        c = Client(**self.kw)
        v = c.create_volume("datos", size_mib=128, host="h1")
        self.assertEqual((v.name, v.size_mib, v.host), ("datos", 128, "h1"))
        self.assertEqual([x.name for x in c.list_volumes()], ["datos"])
        self.assertEqual(c.get_volume("datos").name, "datos")
        c.delete_volume("datos")
        with self.assertRaises(NotFound):
            c.get_volume("datos")

    def test_async(self):
        async def f():
            lote = await AsyncSandbox.create_many(2, **self.kw)
            return [s.id for s in lote], lote.errors
        self.assertEqual(asyncio.run(f()), (["h1/m-1", "h1/m-2"], []))


class TestCiclo(Base):
    def test_create_get_list_renew_delete(self):
        sb = Sandbox.create(template="py", ttl=120, labels={"a": "b"}, **self.kw)
        self.assertEqual(sb.id, "h1/m-1")
        self.assertEqual(sb.info.extra, {"campo_nuevo": 1})
        pet = self.srv.estado.peticiones[-1]
        self.assertEqual(json.loads(pet[4]), {"template": "py", "ttl_seconds": 120, "labels": {"a": "b"}})
        self.assertEqual(Sandbox.connect(sb.id, **self.kw).info.template, "py")
        lista = Sandbox.list(**self.kw)
        self.assertEqual([s.id for s in lista], [sb.id])
        self.assertEqual(lista.partial, "h2: down")
        self.assertEqual(sb.renew(900).ttl_seconds, 900)
        sb.delete()
        with self.assertRaises(NotFound) as cm:
            Sandbox.connect(sb.id, **self.kw)
        self.assertEqual(cm.exception.code, "not_found")
        self.assertEqual(cm.exception.status, 404)

    def test_id_con_barra_sin_codificar(self):
        sb = Sandbox.create(**self.kw)
        sb.refresh()
        self.assertEqual(self.srv.estado.peticiones[-1][1], "/v1/sandboxes/h1/m-1")

    def test_contexto_borra_al_salir(self):
        with Sandbox.create(**self.kw) as sb:
            self.assertIn(sb.id, self.srv.estado.sandboxes)
        self.assertEqual(self.srv.estado.sandboxes, {})

    def test_contexto_borra_si_hay_excepcion(self):
        with self.assertRaises(RuntimeError):
            with Sandbox.create(**self.kw):
                raise RuntimeError("x")
        self.assertEqual(self.srv.estado.sandboxes, {})

    def test_health_sin_token(self):
        self.assertTrue(Client(url=self.srv.url, token="").health())
        self.assertNotIn("Authorization", self.srv.estado.peticiones[-1][3])

    def test_renew_error_400(self):
        sb = Sandbox.create(**self.kw)
        with self.assertRaises(BadRequest):
            sb.renew(-5)


class TestExec(Base):
    def setUp(self):
        super().setUp()
        self.sb = Sandbox.create(**self.kw)

    def test_wait(self):
        r = self.sb.exec("echo hola", cwd="/tmp", env={"A": "1"}, timeout=10, stdin="in")
        self.assertEqual(r.exit_code, 3)
        self.assertEqual(r.stdout, "eco:sh -c echo hola\nin")
        self.assertEqual(r.stderr, "err\n")
        pet = self.srv.estado.peticiones[-1]
        self.assertEqual(pet[2], {"wait": "1"})
        cuerpo = json.loads(pet[4])
        self.assertEqual(cuerpo["cmd"], ["sh", "-c", "echo hola"])
        self.assertEqual((cuerpo["dir"], cuerpo["env"], cuerpo["timeout_seconds"]), ("/tmp", ["A=1"], 10))

    def test_lista_tal_cual(self):
        r = self.sb.exec(["ls", "-l"])
        self.assertTrue(r.stdout.startswith("eco:ls -l"))

    def test_stream(self):
        evs = list(self.sb.exec_stream(["x"]))
        self.assertEqual([e.stream for e in evs], ["stdout", "stderr", None])
        self.assertEqual(evs[0].text, "eco:x\n")
        self.assertEqual(evs[-1].exit, 3)
        self.assertEqual(evs[-1].duration_ms, 5)

    def test_stream_trozos_partidos_a_mitad_de_linea(self):
        import base64
        l1 = (json.dumps({"stream": "stdout", "data": base64.b64encode("ñandú\n".encode()).decode()}) + "\n").encode()
        l2 = (json.dumps({"exit": 0, "duration_ms": 1}) + "\n").encode()
        todo = l1 + l2
        # cortes dentro de la línea, dentro de un escape y entre las dos líneas
        cortes = [7, 20, len(l1) - 1, len(l1), len(l1) + 3]
        trozos, ant = [], 0
        for c in cortes:
            trozos.append(todo[ant:c])
            ant = c
        trozos.append(todo[ant:])
        self.srv.estado.trozos_exec = trozos
        evs = list(self.sb.exec_stream("x"))
        self.assertEqual(len(evs), 2)
        self.assertEqual(evs[0].text, "ñandú\n")
        self.assertEqual(evs[1].exit, 0)

    def test_stream_error_tras_200(self):
        self.srv.estado.error_exec_tras_200 = True
        evs = list(self.sb.exec_stream("x"))
        self.assertEqual(evs[-1].error, "flujo cortado")
        self.assertIsNone(evs[-1].exit)

    def test_stream_error_previo_es_excepcion(self):
        with self.assertRaises(BadRequest):
            self.sb.exec_stream([])

    def test_exec_en_sandbox_inexistente(self):
        self.sb.delete()
        with self.assertRaises(NotFound):
            self.sb.exec("x")


class TestFiles(Base):
    def setUp(self):
        super().setUp()
        self.sb = Sandbox.create(**self.kw)

    def test_binario_ida_y_vuelta(self):
        datos = bytes(range(256)) * 1024 + b"\x00\xff\r\n"
        st = self.sb.files.write("/tmp/b.bin", datos, mode=0o600, mkdir=True)
        self.assertEqual((st.size, st.mode), (len(datos), "0600"))
        pet = self.srv.estado.peticiones[-1]
        self.assertEqual(pet[2], {"path": "/tmp/b.bin", "mode": "0600", "mkdir": "1"})
        self.assertEqual(pet[3]["Content-Type"], "application/octet-stream")
        self.assertEqual(self.sb.files.read("/tmp/b.bin"), datos)
        self.assertEqual(self.sb.files.stat("/tmp/b.bin").size, len(datos))

    def test_list(self):
        self.sb.files.write("/d/a.txt", "x", mkdir=True)
        self.sb.files.write("/d/b.txt", "yy", mode=0o600)
        ls = self.sb.files.list("/d")
        self.assertEqual(ls.path, "/d")
        self.assertEqual([(e.name, e.type, e.size) for e in ls.entries], [("a.txt", "file", 1), ("b.txt", "file", 2)])
        self.assertEqual(ls.entries[1].mode, "0600")
        self.assertEqual(ls.extra, {"nuevo": 1})

    def test_texto_y_delete(self):
        self.sb.files.write("/a b/ñ.txt", "hola")
        self.assertEqual(self.sb.files.read_text("/a b/ñ.txt"), "hola")
        self.sb.files.delete("/a b/ñ.txt")
        with self.assertRaises(NotFound):
            self.sb.files.read("/a b/ñ.txt")


class TestErrores(Base):
    def test_429_retry_after(self):
        self.srv.estado.rate_limit_en_create = 1
        with self.assertRaises(RateLimited) as cm:
            Sandbox.create(**self.kw)
        self.assertEqual(cm.exception.retry_after, 7.0)
        self.assertEqual(cm.exception.status, 429)
        self.assertIsInstance(cm.exception, ApiError)
        Sandbox.create(**self.kw)  # el siguiente pasa

    def test_401(self):
        with self.assertRaises(Unauthorized):
            Sandbox.list(url=self.srv.url, token="otro")

    def test_sin_servidor(self):
        with self.assertRaises(TransportError):
            Sandbox.list(url="http://127.0.0.1:1", token=TOKEN, timeout=2)

    def test_url_invalida(self):
        with self.assertRaises(ConfigError):
            Client(url="ftp://x")


class TestConfigYToken(Base):
    def test_env(self):
        old = {k: os.environ.get(k) for k in ("KLING_SANDBOX_URL", "KLING_SANDBOX_TOKEN")}
        os.environ["KLING_SANDBOX_URL"] = self.srv.url
        os.environ["KLING_SANDBOX_TOKEN"] = TOKEN
        try:
            sb = Sandbox.create()
            self.assertTrue(sb.id)
        finally:
            for k, v in old.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    def test_token_no_sale_en_repr_ni_errores(self):
        sb = Sandbox.create(**self.kw)
        c = sb._client
        for texto in (repr(c), str(c), repr(sb), repr(sb.files), repr(sb.info)):
            self.assertNotIn(TOKEN, texto)
        with self.assertRaises(Unauthorized) as cm:
            Sandbox.list(url=self.srv.url, token="otro-token-xyz")
        self.assertNotIn("otro-token-xyz", str(cm.exception) + repr(cm.exception))
        # error de transporte
        try:
            Sandbox.list(url="http://127.0.0.1:1", token=TOKEN, timeout=2)
        except TransportError as ex:
            self.assertNotIn(TOKEN, str(ex) + repr(ex))

    def test_limpia_token_en_mensaje_del_servidor(self):
        c = Client(url=self.srv.url, token="abc123secreto")
        self.assertNotIn("abc123secreto", c._limpia("falló con abc123secreto dentro"))


class TestAdmin(Base):
    def setUp(self):
        super().setUp()
        self.adm = Admin(**self.kw)

    def test_tenants_ciclo(self):
        t = self.adm.create_tenant("ana", limits={"max_sandboxes": 3})
        self.assertEqual((t.id, t.source, t.read_only, t.limits), ("ana", "store", False, {"max_sandboxes": 3}))
        self.assertEqual(t.extra, {"campo_nuevo": 1})  # tolera campos nuevos
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]),
                         {"name": "ana", "admin": False, "limits": {"max_sandboxes": 3}})
        self.assertEqual([x.id for x in self.adm.list_tenants()], ["ana"])
        u = self.adm.update_tenant("ana", admin=True)
        self.assertTrue(u.admin)
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]), {"admin": True})
        self.assertEqual(self.adm.get_tenant("ana").limits, {"max_sandboxes": 3})
        self.adm.delete_tenant("ana")
        with self.assertRaises(NotFound):
            self.adm.get_tenant("ana")

    def test_claves_crear_rotar_revocar(self):
        self.adm.create_tenant("ana")
        k = self.adm.create_key("ana")
        self.assertTrue(k.key.startswith("ksk_" + k.id))
        n = self.adm.rotate_key("ana", k.id, grace_minutes=5)
        self.assertEqual((n.old_key_id, n.old_key_expires), (k.id, "en 5 min"))
        self.assertNotEqual(n.key, k.key)
        self.assertEqual(json.loads(self.srv.estado.peticiones[-1][4]), {"grace_minutes": 5})
        self.adm.rotate_key("ana", n.id)  # sin cuerpo: la gracia por defecto es del gateway
        self.assertEqual(self.srv.estado.peticiones[-1][4], b"")
        self.adm.revoke_key("ana", n.id)
        fichas = self.adm.get_tenant("ana").keys
        self.assertEqual(len(fichas), 3)
        self.assertIsNotNone(fichas[1].revoked)
        # La lectura no trae secretos.
        self.assertNotIn("key", self.srv.estado.tenants["ana"])
        with self.assertRaises(NotFound):
            self.adm.revoke_key("ana", "nope")

    def test_la_clave_no_sale_en_repr_ni_en_errores(self):
        self.adm.create_tenant("ana")
        k = self.adm.create_key("ana")
        self.assertNotIn(k.key, repr(k) + str(k))
        self.assertNotIn(TOKEN, repr(self.adm.client))

    def test_sin_admin_es_forbidden(self):
        with self.assertRaises(Forbidden):
            Admin(url=self.srv.url, token=TOKEN_USUARIO).list_tenants()


class TestSuperficieNueva(Base):
    """Lo que el gateway añadió en la ronda 6: listado con filtros, create(env),
    pause/resume/egress, endpoints, código, ficheros nuevos, reintentos."""

    def test_list_con_filtros_y_cursor(self):
        for i in range(3):
            Sandbox.create(labels={"k": "v" if i < 2 else "w"}, **self.kw)
        self.assertEqual(len(Sandbox.list(label={"k": "v"}, **self.kw)), 2)
        self.assertEqual(self.srv.estado.peticiones[-1][2], {"label": "k=v"})
        self.assertEqual(len(Sandbox.list(state="paused", **self.kw)), 0)
        pag = Sandbox.list(limit=2, **self.kw)
        self.assertEqual((len(pag), pag.next_cursor, pag.partial), (2, "h1/m-2", "h2: down"))
        resto = Sandbox.list(limit=2, cursor=pag.next_cursor, **self.kw)
        self.assertEqual((len(resto), resto.next_cursor), (1, None))

    def test_create_con_env(self):
        Sandbox.create(env={"A": "1"}, **self.kw)
        self.assertEqual(self.srv.estado.env_de_creacion, {"A": "1"})

    def test_pause_resume_egress(self):
        sb = Sandbox.create(**self.kw)
        self.assertEqual(sb.pause().state, "paused")
        self.assertEqual(sb.info.state, "paused")
        self.assertEqual(sb.resume().state, "running")
        pol = sb.set_egress("allowlist", ["a.example.test"])
        self.assertEqual((pol.egress, pol.allow_domains, pol.applied), ("allowlist", ["a.example.test"], "live"))
        ult = self.srv.estado.peticiones[-1]
        self.assertEqual((ult[0], ult[1]), ("PATCH", "/v1/sandboxes/h1/m-1/egress"))
        self.assertEqual(json.loads(ult[4]), {"egress": "allowlist", "allow_domains": ["a.example.test"]})

    def test_endpoints(self):
        sb = Sandbox.create(**self.kw)
        ep = sb.endpoints.create(8000, ttl=60)
        self.assertEqual((ep.port, ep.token, ep.public), (8000, "tok-ep", False))
        pub = sb.endpoints.create(9000, public=True)
        self.assertEqual((pub.public, pub.token), (True, ""))
        self.assertEqual([e.port for e in sb.endpoints.list()], [8000, 9000])
        sb.endpoints.revoke(8000)
        self.assertEqual([e.port for e in sb.endpoints.list()], [9000])
        with self.assertRaises(NotFound):
            sb.endpoints.revoke(8000)

    def test_code_run_acaba_en_done(self):
        sb = Sandbox.create(**self.kw)
        evs = list(sb.code.run("1+1", language="python", timeout=5))
        self.assertEqual([e.type for e in evs], ["stdout", "result", "done"])
        self.assertEqual(evs[1].results, {"text/plain": "2"})
        self.assertEqual(self.srv.estado.codigo_pedido,
                         {"code": "1+1", "language": "python", "timeout_seconds": 5})
        r = sb.code.run_wait("fallo()")
        self.assertEqual((r.ok, r.stdout, r.error.ename), (False, "hola\n", "NameError"))
        r = sb.code.run_wait("1+1")
        self.assertEqual((r.ok, r.results["text/plain"]), (True, "2"))

    def test_contextos_e_interrumpir(self):
        sb = Sandbox.create(**self.kw)
        c = sb.code.contexts.create(language="bash", cwd="/w")
        self.assertEqual((c.language, c.cwd), ("bash", "/w"))
        self.assertEqual([x.id for x in sb.code.contexts.list()], [c.id])
        self.assertEqual(sb.code.contexts.get(c.id).id, c.id)
        list(sb.code.run("x", context=c.id))
        self.assertEqual(self.srv.estado.codigo_pedido["context_id"], c.id)
        sb.code.interrupt(c.id)
        self.assertEqual(self.srv.estado.interrumpidos, [(sb.id, c.id)])
        sb.code.contexts.delete(c.id)
        with self.assertRaises(NotFound):
            sb.code.contexts.get(c.id)

    def test_ficheros_nuevos(self):
        sb = Sandbox.create(**self.kw)
        sb.files.write("/d/a.log", "0123456789")
        sb.files.write("/d/b.txt", "x")
        self.assertEqual(sb.files.read("/d/a.log", range=(2, 5)), b"2345")
        self.assertEqual(sb.files.read("/d/a.log", range=(7, None)), b"789")
        self.assertEqual(sb.files.read("/d/a.log", range="bytes=-3"), b"789")
        with self.assertRaises(ApiError) as cm:
            sb.files.read("/d/a.log", range=(50, None))
        self.assertEqual((cm.exception.status, cm.exception.code), (416, "requested_range_not_satisfiable"))
        self.assertEqual(sb.files.search("/d", "*.log").matches, ["/d/a.log"])
        sb.files.move("/d/b.txt", "/d/c.txt")
        with self.assertRaises(NotFound):
            sb.files.stat("/d/b.txt")
        sb.files.write("/d/b.txt", "y")
        with self.assertRaises(Conflict):
            sb.files.move("/d/b.txt", "/d/c.txt")
        sb.files.move("/d/b.txt", "/d/c.txt", overwrite=True)
        self.assertEqual(sb.files.read_text("/d/c.txt"), "y")
        self.assertTrue(sb.files.mkdir("/d/sub/x").is_dir)

    def test_tar_de_subida_y_bajada(self):
        import io
        import tarfile
        sb = Sandbox.create(**self.kw)
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tf:
            ti = tarfile.TarInfo("a.txt")
            ti.size = 2
            tf.addfile(ti, io.BytesIO(b"hi"))
        r = sb.files.upload_tar("/w", buf.getvalue(), mkdir=True)
        self.assertEqual(r.entries, 1)
        self.assertEqual(self.srv.estado.peticiones[-1][2], {"path": "/w", "tar": "1", "mkdir": "1"})
        self.assertEqual(sb.files.read("/w/a.txt"), b"hi")
        with tarfile.open(fileobj=io.BytesIO(sb.files.download_tar("/w"))) as tf:
            self.assertEqual(tf.extractfile("a.txt").read(), b"hi")

    def test_api_error_trae_code(self):
        with self.assertRaises(NotFound) as cm:
            Sandbox.connect("h1/nada", **self.kw)
        self.assertEqual(cm.exception.code, "not_found")

    def test_reintento_con_retry_after(self):
        sb = Sandbox.create(**self.kw)
        # Sin reintentos (por defecto) el 503 sube tal cual.
        self.srv.estado.fallos_503 = 1
        with self.assertRaises(ServerError) as cm:
            sb.renew()
        self.assertEqual((cm.exception.status, cm.exception.retry_after, cm.exception.code), (503, 2.0, "service_unavailable"))
        # Con reintentos espera el Retry-After (2 s, sin dormir de verdad) y sale.
        cliente = Client(url=self.srv.url, token=TOKEN, retries=2)
        pausas = []
        cliente._dormir = pausas.append
        self.srv.estado.fallos_503 = 2
        cliente.renew_sandbox(sb.id)
        self.assertEqual(pausas, [2.0, 2.0])
        # Agotados los reintentos, lanza.
        self.srv.estado.fallos_503 = 5
        with self.assertRaises(ServerError):
            cliente.renew_sandbox(sb.id)
        # Un 429 de cuota al crear también se reintenta.
        self.srv.estado.rate_limit_en_create = 1
        pausas.clear()
        cliente.create_sandbox()
        self.assertEqual(pausas, [7.0])

    def test_url_por_defecto(self):
        self.assertEqual(Client().url, "http://127.0.0.1:8090")

    def test_async_superficie_nueva(self):
        async def main():
            async with await AsyncSandbox.create(**self.kw) as sb:
                await sb.pause()
                self.assertEqual((await sb.resume()).state, "running")
                self.assertEqual((await sb.endpoints.create(80)).port, 80)
                evs = [e async for e in await sb.code.run("1+1")]
                self.assertEqual(evs[-1].type, "done")
                await sb.files.mkdir("/z")
                self.assertEqual((await sb.files.search("/", "*")).matches, [])
        asyncio.run(main())


class TestAsync(Base):
    def test_ciclo_asincrono(self):
        async def main():
            async with await AsyncSandbox.create(template="py", **self.kw) as sb:
                r = await sb.exec("x")
                self.assertEqual(r.exit_code, 3)
                evs = [e async for e in await sb.exec_stream(["y"])]
                self.assertEqual(evs[-1].exit, 3)
                await sb.files.write("/f", b"\x00\x01")
                self.assertEqual(await sb.files.read("/f"), b"\x00\x01")
                self.assertEqual(len(await AsyncSandbox.list(**self.kw)), 1)
            self.assertEqual(self.srv.estado.sandboxes, {})
        asyncio.run(main())


if __name__ == "__main__":
    unittest.main()
