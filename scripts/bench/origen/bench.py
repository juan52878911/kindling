#!/usr/bin/env python3
# Banco kindling vs OpenSandbox en el mismo host (CT 105).
# Uso: bench.py k|kpool|osb  -> imprime una línea JSON con los resultados.
import asyncio, json, os, statistics, subprocess, sys, time
from concurrent.futures import ThreadPoolExecutor

MODO = sys.argv[1]
SERIE = int(os.environ.get("SERIE", "15"))
CONC = [int(x) for x in os.environ.get("CONC", "10,20").split(",")]
EXECS = int(os.environ.get("EXECS", "30"))
ESPERA_POOL = int(os.environ.get("ESPERA_POOL", "0"))


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
def bench_k(plantilla):
    sys.path.insert(0, "/root/bench2/sdk")
    from kindling_sandbox import Sandbox
    url, tok = "http://127.0.0.1:18099", os.environ["KTOK"]

    def crear():
        t = time.perf_counter()
        s = Sandbox.create(template=plantilla, ttl=900, url=url, token=tok)
        return s, (time.perf_counter() - t) * 1000

    def borrar(s):
        try:
            s.delete()
        except Exception:
            pass

    res = {}
    s, _ = crear(); borrar(s)  # calentamiento, no cuenta
    serie = []
    for _ in range(SERIE):
        s, ms = crear()
        serie.append(ms)
        borrar(s)
    res["create_serial_ms"] = resumen(serie)

    for c in CONC:
        time.sleep(ESPERA_POOL)  # dejar que el pool se rellene antes de la ráfaga
        ok, err, ts = [], 0, []
        m0 = mem_usada_mib()
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
        res[f"create_c{c}_ms"] = resumen(ts) | {"fallos": err}
        if ok and m0 is not None:
            res[f"mem_por_sandbox_c{c}_mib"] = round((m1 - m0) / len(ok), 1)
        if c == CONC[0] and ok:
            s = ok[0]
            ex_ms = []
            for _ in range(EXECS):
                t = time.perf_counter()
                r = s.exec(["echo", "ack"])
                ex_ms.append((time.perf_counter() - t) * 1000)
                assert r.stdout.strip() == "ack", r
            res["exec_echo_ms"] = resumen(ex_ms)
            # pausa/reanudar con el CLI del daemon (el frontal no lo expone)
            env = dict(os.environ, KLING_HOST="unix:///run/kt-bench/kling.sock",
                       KLING_CONFIG="/srv/kt-bench/config.json")
            K = "/srv/kt-bench/bin/kling"
            mid = s.id.split("/")[-1]
            for parar, clave in (("pause", "resume_hasta_exec_ms"), ("freeze", "thaw_desde_freeze_hasta_exec_ms")):
                pr = []
                for _ in range(5):
                    subprocess.run([K, parar, mid], env=env, check=True, capture_output=True)
                    t = time.perf_counter()
                    subprocess.run([K, "thaw", mid], env=env, check=True, capture_output=True)
                    r = s.exec(["echo", "ack"])
                    pr.append((time.perf_counter() - t) * 1000)
                res[clave] = resumen(pr)
        with ThreadPoolExecutor(c) as ex:
            list(ex.map(borrar, ok))
        time.sleep(2)
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


if MODO == "k":
    out = bench_k("bench")
elif MODO == "kpool":
    out = bench_k("bench-pool")
elif MODO == "osbpool":
    out = asyncio.run(bench_osb(pool=os.environ.get("POOL", "bench")))
else:
    out = asyncio.run(bench_osb())
print(json.dumps({"modo": MODO} | out))
