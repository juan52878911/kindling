#!/usr/bin/env python3
# Banco kindling vs OpenSandbox en el mismo host.
# Uso: bench.py k|kpool|osb|osbpool  -> imprime una línea JSON con serie, c10/c20/c50 y la
# tabla de fases (solo en k/kpool: deltas de /metrics del daemon y del gateway por tanda).
# bench.py --self-test: comprueba claves y orden de las medidas con un gateway falso (sin KVM).
# Entorno: KTOK (token del inquilino, obligatorio en k/kpool), SERIE, CONC, EXECS, ESPERA_POOL,
# KB_SDK (ruta del SDK Python), KB_URL (gateway), KB_DAEMON_SOCK, KB_DAEMON_CONFIG, KB_KLING (CLI).
# Sin dependencias fuera de la librería estándar (y el SDK de cada sistema).
import asyncio, importlib.util, json, os, re, subprocess, sys, time
from concurrent.futures import ThreadPoolExecutor

MODO = sys.argv[1] if len(sys.argv) > 1 else ""
SERIE = int(os.environ.get("SERIE", "15"))
CONC = [int(x) for x in os.environ.get("CONC", "10,20").split(",")]  # "…,50,50": dos c50 por separado
EXECS = int(os.environ.get("EXECS", "30"))
ESPERA_POOL = int(os.environ.get("ESPERA_POOL", "0"))
E5_N = int(os.environ.get("E5_N", "5"))  # sandboxes por variante de pausa→exec (E5)
E5_OCIOSA_S = int(os.environ.get("E5_OCIOSA_S", "60"))  # reposo antes de la pausa "ociosa"
# KLING_NET_POOL lo lee el daemon (daemon-up.sh hereda el entorno); aquí solo se anota en el JSON.
NET_POOL = os.environ.get("KLING_NET_POOL", "")
KVM_DIR = os.environ.get("KB_KVM_DEBUGFS", "/sys/kernel/debug/kvm")
KB = os.environ.get("KB", "/opt/kbench")
KB_URL = os.environ.get("KB_URL", "http://127.0.0.1:18099")
SOCK = os.environ.get("KB_DAEMON_SOCK", "/run/kt-bench/kling.sock")
KB_CONFIG = os.environ.get("KB_DAEMON_CONFIG", "/srv/kt-bench/config.json")
KLING = os.environ.get("KB_KLING", "/srv/kt-bench/bin/kling")


def cargar_fases():
    # python -I no pone el directorio del script en sys.path: se carga por ruta.
    ruta = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fases.py")
    esp = importlib.util.spec_from_file_location("fases", ruta)
    m = importlib.util.module_from_spec(esp)
    esp.loader.exec_module(m)
    return m


def pct(v, p):
    if not v:
        return None
    v = sorted(v)
    return round(v[min(len(v) - 1, int(round(p / 100 * (len(v) - 1))))], 1)


def resumen(v):
    return {"n": len(v), "p50": pct(v, 50), "p95": pct(v, 95), "max": round(max(v), 1) if v else None}


def mem_usada_mib():
    # Lo que usa el CT entero (cgroup), no solo un proceso.
    for f in ("/sys/fs/cgroup/memory.current",):
        try:
            return int(open(f).read()) // (1 << 20)
        except OSError:
            pass
    # En una VM no hay cgroup del CT: lo usado según /proc/meminfo (como bench_k8s.py).
    m = {l.split(":")[0]: int(l.split()[1]) for l in open("/proc/meminfo")}
    return (m["MemTotal"] - m["MemAvailable"]) // 1024


# ---------------- kindling ----------------
UNIDAD = os.environ.get("KB_UNIT", "kt-bench")
RESYNC_RUTA = os.environ.get("KB_RESYNC_RUTA", "/machines/{id}/resync")  # GET en el socket del daemon


def clave_tanda(c, vistas):
    # Dos ráfagas del mismo tamaño no se mezclan: c50, c50b, c50c...
    n = vistas.get(c, 0)
    vistas[c] = n + 1
    return f"c{c}" + ("" if n == 0 else chr(ord("a") + n))


def _ejecutar(cmd, **kw):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=20, **kw).stdout
    except Exception as e:  # el diagnóstico nunca rompe la medición
        return f"<error: {str(e)[:100]}>"


def vmm_muertos_desde(t_unix):
    # Líneas `watch:` del journal del daemon desde que empezó la tanda: un VMM que muere o un
    # huérfano que el vigilante mata contamina la tanda y tiene que verse en el JSON.
    out = _ejecutar(["journalctl", "-u", UNIDAD, "--no-pager", "-o", "cat", "--since", f"@{int(t_unix)}"])
    lin = [l for l in out.splitlines() if "watch:" in l]
    muertos = [l for l in lin if "exited" in l or "killing orphan" in l]
    return {"watch": len(lin), "n": len(muertos), "muestra": [l[:160] for l in muertos[:3]]}


def memory_events():
    try:
        return open("/sys/fs/cgroup/memory.events").read().split()
    except OSError:
        return None


def cerrar_tanda(res, nombre, t_unix):
    # Al final de cada tanda: muertes de VMM, dmesg (OOM, errores de disco) y memory.events.
    res.setdefault("vmm_muertos", {})[nombre] = vmm_muertos_desde(t_unix)
    d = _ejecutar(["dmesg", "--ctime"]).splitlines()
    cola = [l[:160] for l in d if re.search(r"oom|killed process|i/o error|xfs|fail", l, re.I)][-5:]
    ev = memory_events()
    res.setdefault("diag", {})[nombre] = {
        "dmesg": cola, "memory_events": dict(zip(ev[::2], ev[1::2])) if ev else None}


def resync_agente(sock, mid):
    # GET /resync del agente tras una creación en serie: el cronómetro del último arranque
    # (critico_us y pasos) para ver a qué paso del invitado va a parar la cola.
    try:
        c = F_UNIX(sock)
        c.request("GET", RESYNC_RUTA.format(id=mid))
        r = c.getresponse()
        cuerpo = r.read().decode()
        c.close()
        if r.status != 200:
            return {"error": f"HTTP {r.status}"}
        d = json.loads(cuerpo)
        return {k: v for k, v in d.items() if k.endswith("_us") or k in ("restore", "diferido_hecho")}
    except Exception as e:
        return {"error": str(e)[:100]}


def kvm_contadores():
    # pf_fixed y exits de KVM (debugfs, root): None si no se pueden leer.
    try:
        return {k: int(open(os.path.join(KVM_DIR, k)).read()) for k in ("pf_fixed", "exits")}
    except (OSError, ValueError):
        return None


def kvm_delta(a, b):
    if a is None or b is None:
        return None
    return {k: b[k] - a[k] for k in a}


def pausa_exec(s, kling_cli, env, mid):
    # Una pausa→exec: ms desde `thaw` hasta que responde el primer exec.
    subprocess.run([kling_cli, "pause", mid], env=env, check=True, capture_output=True)
    t = time.perf_counter()
    subprocess.run([kling_cli, "thaw", mid], env=env, check=True, capture_output=True)
    s.exec(["echo", "ack"])
    return (time.perf_counter() - t) * 1000


def medir_pausa_e5(crear, borrar, res, kling_cli, env):
    # E5: pausa→exec con un sandbox recién creado (sin ejecutar nada antes) y con uno ocioso
    # E5_OCIOSA_S s. Un sandbox por muestra; los ociosos se crean juntos y esperan a la vez.
    nuevas = []
    for _ in range(E5_N):
        s, _ms = crear()
        nuevas.append(pausa_exec(s, kling_cli, env, s.id.split("/")[-1]))
        borrar(s)
    res["pausa_exec_nueva_ms"] = resumen(nuevas)
    ociosas = [crear()[0] for _ in range(E5_N)]
    time.sleep(E5_OCIOSA_S)
    oc = [pausa_exec(s, kling_cli, env, s.id.split("/")[-1]) for s in ociosas]
    for s in ociosas:
        borrar(s)
    res["pausa_exec_ociosa_ms"] = resumen(oc) | {"ocioso_s": E5_OCIOSA_S}


def medir_ciclo(s, res, sufijo, kling_cli, env, mid, execs):
    # Exec y pausa/congelar sobre un sandbox. `sufijo` dice en qué momento: "rafaga" (justo al
    # volver la ráfaga, con el relleno del pool en marcha) o "reposo" (pool ya relleno).
    ex_ms = []
    for _ in range(execs):
        t = time.perf_counter()
        r = s.exec(["echo", "ack"])
        ex_ms.append((time.perf_counter() - t) * 1000)
        assert r.stdout.strip() == "ack", r
    res[f"exec_echo_{sufijo}_ms"] = resumen(ex_ms)
    for parar, clave in (("pause", "resume_hasta_exec"), ("freeze", "thaw_desde_freeze_hasta_exec")):
        pr = []
        for _ in range(5):
            subprocess.run([kling_cli, parar, mid], env=env, check=True, capture_output=True)
            t = time.perf_counter()
            subprocess.run([kling_cli, "thaw", mid], env=env, check=True, capture_output=True)
            s.exec(["echo", "ack"])
            pr.append((time.perf_counter() - t) * 1000)
        res[f"{clave}_{sufijo}_ms"] = resumen(pr)


def bench_k(plantilla, Sandbox=None, F=None):
    if Sandbox is None:
        sys.path.insert(0, os.environ.get("KB_SDK", os.path.join(KB, "sdk")))
        from kindling_sandbox import Sandbox
    url, tok = KB_URL, os.environ.get("KTOK", "")
    F = F or cargar_fases()
    fuentes = F.fuentes(KB_URL, tok, SOCK)

    def crear():
        t = time.perf_counter()
        s = Sandbox.create(template=plantilla, ttl=900, url=url, token=tok)
        return s, (time.perf_counter() - t) * 1000

    def borrar(s):
        try:
            s.delete()
        except Exception:
            pass

    res = {"fases": {}, "net_pool": NET_POOL}
    s, _ = crear(); borrar(s)  # calentamiento, no cuenta
    serie, resyncs, kvm = [], [], []
    antes = F.leer(fuentes)
    t0 = time.time()
    for _ in range(SERIE):
        k0 = kvm_contadores()
        s, ms = crear()
        kvm.append(kvm_delta(k0, kvm_contadores()))
        serie.append(ms)
        resyncs.append(resync_agente(SOCK, s.id.split("/")[-1]))  # antes de borrarla
        borrar(s)
    res["create_serial_ms"] = resumen(serie)
    res["resync_serie"] = resyncs
    res["kvm_serie"] = kvm  # delta de pf_fixed/exits por creación (None sin debugfs)
    res["fases"]["serie"] = F.tabla(antes, F.leer(fuentes), serie)
    cerrar_tanda(res, "serie", t0)

    vistas = {}
    for i, c in enumerate(CONC):
        nombre = clave_tanda(c, vistas)
        time.sleep(ESPERA_POOL)  # dejar que el pool se rellene antes de la ráfaga
        ok, err, ts = [], 0, []
        antes = F.leer(fuentes)
        m0 = mem_usada_mib()
        t0 = time.time()
        with ThreadPoolExecutor(c) as ex:
            futs = [ex.submit(crear) for _ in range(c)]
            for f in futs:
                try:
                    s, ms = f.result()
                    ok.append(s); ts.append(ms)
                except Exception as e:
                    err += 1
                    res.setdefault("errores", []).append(str(e)[:120])
        m1 = mem_usada_mib()
        res["fases"][nombre] = F.tabla(antes, F.leer(fuentes), ts)
        res[f"create_{nombre}_ms"] = resumen(ts) | {"fallos": err}
        if ok and m0 is not None:
            res[f"mem_por_sandbox_{nombre}_mib"] = round((m1 - m0) / len(ok), 1)
        if i == 0 and ok:
            s = ok[0]
            env = dict(os.environ, KLING_HOST="unix://" + SOCK, KLING_CONFIG=KB_CONFIG)
            mid = s.id.split("/")[-1]
            # 1) justo al volver la ráfaga: el relleno del pool compite por CPU (medida "durante").
            medir_ciclo(s, res, "rafaga", KLING, env, mid, EXECS)
            # 2) con el pool en reposo: lo que ve un usuario cuando nadie reclama.
            time.sleep(ESPERA_POOL)
            medir_ciclo(s, res, "reposo", KLING, env, mid, EXECS)
            # Claves de siempre = justo tras la ráfaga, para comparar con las rondas anteriores.
            for k in ("exec_echo", "resume_hasta_exec", "thaw_desde_freeze_hasta_exec"):
                res[f"{k}_ms"] = res[f"{k}_rafaga_ms"]
        with ThreadPoolExecutor(c) as ex:
            list(ex.map(borrar, ok))
        cerrar_tanda(res, nombre, t0)
        time.sleep(2)
    env = dict(os.environ, KLING_HOST="unix://" + SOCK, KLING_CONFIG=KB_CONFIG)
    medir_pausa_e5(crear, borrar, res, KLING, env)
    return res


# ---------------- OpenSandbox ----------------
async def bench_osb(pool=None):
    from datetime import timedelta
    from opensandbox import Sandbox
    from opensandbox.config import ConnectionConfig
    cc = ConnectionConfig(domain="127.0.0.1:18080")

    async def crear():
        t = time.perf_counter()
        if pool:  # pod precalentado del Pool CRD (runtime kubernetes)
            s = await Sandbox.create("alpine:3.20", connection_config=cc, extensions={"poolRef": pool},
                                     timeout=timedelta(seconds=900), ready_timeout=timedelta(seconds=120))
        else:
            # Mismo tamaño que la plantilla de kindling (256 MiB); en k8s el valor por
            # defecto de OpenSandbox es request=limit 1 CPU / 2 GiB.
            s = await Sandbox.create("alpine:3.20", connection_config=cc,
                                     resource={"cpu": "250m", "memory": "256Mi"},
                                     timeout=timedelta(seconds=900), ready_timeout=timedelta(seconds=120))
        return s, (time.perf_counter() - t) * 1000

    async def borrar(s):
        try:
            await s.kill()
        except Exception:
            try:
                await s.destroy()
            except Exception:
                pass

    res = {}
    s, _ = await crear(); await borrar(s)  # calentamiento, no cuenta
    serie = []
    for _ in range(SERIE):
        s, ms = await crear()
        serie.append(ms)
        await borrar(s)
    res["create_serial_ms"] = resumen(serie)

    for c in CONC:
        await asyncio.sleep(ESPERA_POOL)
        m0 = mem_usada_mib()
        rs = await asyncio.gather(*[crear() for _ in range(c)], return_exceptions=True)
        m1 = mem_usada_mib()
        ok = [r[0] for r in rs if not isinstance(r, BaseException)]
        ts = [r[1] for r in rs if not isinstance(r, BaseException)]
        errs = [str(r)[:120] for r in rs if isinstance(r, BaseException)]
        if errs:
            res.setdefault("errores", []).extend(errs)
        res[f"create_c{c}_ms"] = resumen(ts) | {"fallos": len(errs)}
        if ok and m0 is not None:
            res[f"mem_por_sandbox_c{c}_mib"] = round((m1 - m0) / len(ok), 1)
        if c == CONC[0] and ok:
            s = ok[0]
            ex_ms = []
            for _ in range(EXECS):
                t = time.perf_counter()
                e = await s.commands.run("echo ack")
                ex_ms.append((time.perf_counter() - t) * 1000)
                assert e.logs.stdout[0].text.strip() == "ack"
            res["exec_echo_ms"] = resumen(ex_ms)
            pr = []
            try:
                if os.environ.get("SIN_PAUSA"):
                    raise RuntimeError("pausa no medida (en k8s hace commit+push de la imagen a un registro)")
                for _ in range(5):
                    await s.pause()
                    t = time.perf_counter()
                    s = await Sandbox.resume(s.id, connection_config=cc)
                    await s.commands.run("echo ack")
                    pr.append((time.perf_counter() - t) * 1000)
                res["resume_hasta_exec_ms"] = resumen(pr)
            except Exception as e:
                res["resume_hasta_exec_ms"] = {"error": str(e)[:160]}
        await asyncio.gather(*[borrar(s) for s in ok])
        await asyncio.sleep(2)
    return res


def F_UNIX(ruta):
    return cargar_fases()._Unix(ruta)


def self_test():
    # Gateway falso: Sandbox y fases de mentira, y un registro del orden en que se mide.
    global SERIE, CONC, EXECS, ESPERA_POOL, E5_N, E5_OCIOSA_S, subprocess
    import types
    SERIE, CONC, EXECS, ESPERA_POOL, E5_N, E5_OCIOSA_S = 2, [2, 3, 3], 2, 0, 2, 0
    ev = []

    class R:
        stdout = "ack\n"

    class Fake:
        n = 0

        def __init__(self):
            Fake.n += 1
            self.id = f"t/fake{Fake.n}"

        @classmethod
        def create(cls, **kw):
            ev.append("create")
            return cls()

        def delete(self):
            ev.append("delete")

        def exec(self, cmd):
            ev.append("exec")
            return R()

    class FF:
        def fuentes(self, *a):
            return {}

        def leer(self, f):
            return {}

        def tabla(self, a, b, v):
            return {"n": len(v)}

    def run(cmd, **kw):
        ev.append(cmd[1])
        return types.SimpleNamespace(stdout="", returncode=0)
    globals()["resync_agente"] = lambda sock, mid: {"critico_us": 7}
    globals()["_ejecutar"] = lambda cmd, **kw: ""
    sub = subprocess.run
    subprocess.run = run
    try:
        out = bench_k("bench", Fake, FF())
    finally:
        subprocess.run = sub
    claves = list(out)
    for k in ("create_serial_ms", "create_c2_ms", "create_c3_ms", "create_c3b_ms", "exec_echo_rafaga_ms",
              "exec_echo_reposo_ms", "resume_hasta_exec_reposo_ms", "thaw_desde_freeze_hasta_exec_rafaga_ms",
              "exec_echo_ms", "vmm_muertos", "diag", "resync_serie", "kvm_serie", "net_pool",
              "pausa_exec_nueva_ms", "pausa_exec_ociosa_ms"):
        assert k in out, f"falta {k}: {claves}"
    assert out["resync_serie"] == [{"critico_us": 7}] * 2
    assert len(out["kvm_serie"]) == 2 and out["pausa_exec_nueva_ms"]["n"] == 2, out
    assert out["pausa_exec_ociosa_ms"]["n"] == 2, out
    assert set(out["vmm_muertos"]) == {"serie", "c2", "c3", "c3b"}, out["vmm_muertos"]
    assert claves.index("create_c3_ms") < claves.index("create_c3b_ms")
    # Orden: ráfaga → ciclo "rafaga" (exec, pause, thaw, freeze, thaw) → ciclo "reposo".
    pos = [i for i, e in enumerate(ev) if e == "pause"]
    assert len(pos) == 14 and ev.count("freeze") == 10, ev
    assert ev.index("pause") > ev.index("exec") and ev.index("freeze") > ev.index("pause")
    print("self-test ok")


if MODO == "--self-test":
    self_test()
    sys.exit(0)
if MODO == "k":
    out = bench_k("bench")
elif MODO == "kpool":
    out = bench_k("bench-pool")
elif MODO == "osbpool":
    out = asyncio.run(bench_osb(pool=os.environ.get("POOL", "bench")))
else:
    out = asyncio.run(bench_osb())
print(json.dumps({"modo": MODO} | out))
