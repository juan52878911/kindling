#!/usr/bin/env python3
"""Resumen del banco de IA (scripts/96-von-bench.sh, esquema von-bench/2).

    python3 -I ia.py res/ia-*.json          resumen legible y los criterios de P1, P2 y P3
    python3 -I ia.py --validar f.json...    sale con 1 si falta alguna clave
    python3 -I ia.py --self-test            se comprueba a sí mismo con un JSON inventado

Los criterios son los de analisis/ronda8-ia/plan.md §4. Solo biblioteca estándar.
"""
import json
import math
import sys

ESQUEMA = "von-bench/2"
# Claves de primer nivel y, para las secciones medidas, las que deben llevar dentro.
CLAVES = {
    "esquema": None,
    "modelo": ["snapshot", "contrato", "mem_mib", "snapshot_mem_mib"],
    "host": ["uname", "drop_caches", "secciones"],
    "frio": ["arranque_ms", "hasta_primera_ms"],
    "restaurar": ["thaw_ms", "hasta_primera_ms", "peticion_ms"],
    "primera": ["restaurar_caliente", "restaurar_fria", "descongelar_caliente", "descongelar_fria"],
    "tps": ["prompt_tok_s", "generacion_tok_s"],
    "memoria": ["restauradas", "descongeladas", "remove"],
    "ociosa": ["descongelar_ms", "restaurar_ms", "gateway"],
    "semillas": ["iguales"],
    "carga": ["ruta", "gateway", "directo"],
}
PUNTO = ["replicas", "pss_total_mib", "pss_mib", "rss_total_mib", "host"]


def omitida(v):
    return isinstance(v, dict) and "omitida" in v


def faltan(doc):
    """Lista de claves que faltan (vacía = completo)."""
    out = []
    if doc.get("esquema") != ESQUEMA:
        out.append("esquema (%r, quería %r)" % (doc.get("esquema"), ESQUEMA))
    for k, dentro in CLAVES.items():
        if k not in doc:
            out.append(k)
            continue
        v = doc[k]
        if dentro is None or omitida(v):
            continue
        if not isinstance(v, dict):
            out.append("%s (no es un objeto)" % k)
            continue
        out += ["%s.%s" % (k, d) for d in dentro if d not in v]
    mem = doc.get("memoria")
    if isinstance(mem, dict) and not omitida(mem):
        for var in ("restauradas", "descongeladas", "remove"):
            pts = mem.get(var)
            if isinstance(pts, list):
                for i, p in enumerate(pts):
                    out += ["memoria.%s[%d].%s" % (var, i, c) for c in PUNTO if c not in p]
    return out


def pct(xs, p):
    """Percentil de rango más cercano (el mismo que ia-bench)."""
    xs = sorted(x for x in xs if isinstance(x, (int, float)))
    if not xs:
        return None
    if p <= 0:
        return xs[0]
    i = min(len(xs) - 1, max(0, math.ceil(len(xs) * p / 100) - 1))
    return xs[i]


def fmt(x, suf=" ms"):
    if x is None:
        return "n/a"
    if isinstance(x, float) and not x.is_integer():
        return "%.1f%s" % (x, suf)
    return "%d%s" % (x, suf)


def est(xs):
    return "p50 %s  p95 %s  (n=%d)" % (fmt(pct(xs, 50)), fmt(pct(xs, 95)), len([x for x in xs if x is not None]))


def muestras(v, campo):
    if not isinstance(v, list):
        return []
    return [m.get(campo) for m in v if isinstance(m, dict)]


def marginal(pts):
    """PSS de la réplica adicional: (PSS con N − PSS con 1) / (N − 1), con el último N medido."""
    pts = [p for p in pts if isinstance(p, dict) and p.get("pss_total_mib") is not None]
    if not pts:
        return None, None
    uno = [p for p in pts if p.get("replicas") == 1]
    n = max(p["replicas"] for p in pts)
    alto = [p for p in pts if p["replicas"] == n]
    if not uno or n < 2:
        return (uno[0]["pss_total_mib"] if uno else None), None
    p1 = pct([p["pss_total_mib"] for p in uno], 50)
    pn = pct([p["pss_total_mib"] for p in alto], 50)
    return p1, (pn - p1) / (n - 1)


def resumen(doc, out):
    w = out.append
    m = doc.get("modelo", {})
    modelo_mib = m.get("snapshot_mem_mib") or m.get("mem_mib") or 0
    w("== %s  (%s, contrato %s, %s vCPU, %s MiB, dorado %s MiB en memoria, backend %s)" % (
        m.get("snapshot"), m.get("modelo"), m.get("contrato"), m.get("vcpus"), m.get("mem_mib"),
        m.get("snapshot_mem_mib"), m.get("backend")))
    h = doc.get("host", {})
    w("   host %s, %s CPU, %s MiB; drop_caches %s%s" % (h.get("uname"), h.get("cpus"), h.get("mem_total_mib"),
      "sí" if h.get("drop_caches") else "no", "" if h.get("drop_caches") else " (%s)" % h.get("drop_caches_motivo")))
    veredictos = []

    fr = doc.get("frio")
    if isinstance(fr, dict) and not omitida(fr):
        w("frío: arranque %s, hasta la primera respuesta %s" % (fmt(fr.get("arranque_ms")), fmt(fr.get("hasta_primera_ms"))))

    pr = doc.get("primera")
    if isinstance(pr, dict) and not omitida(pr):
        w("primera respuesta (P1):")
        for k, et in (("restaurar_caliente", "restaurar, caché caliente"), ("restaurar_fria", "restaurar, tras drop_caches"),
                      ("descongelar_caliente", "descongelar, caché caliente"), ("descongelar_fria", "descongelar, tras drop_caches")):
            v = pr.get(k)
            if omitida(v):
                w("  %-30s omitida: %s" % (et, v["omitida"]))
                continue
            maj = [f.get("majflt") for f in muestras(v, "fallos") if isinstance(f, dict)]
            w("  %-30s %s   petición %s   resync %s   majflt p50 %s" % (
                et, est(muestras(v, "total_ms")), fmt(pct(muestras(v, "peticion_ms"), 50)),
                fmt(pct(muestras(v, "resync_ms"), 50)), fmt(pct(maj, 50), "")))
        cal = pct(muestras(pr.get("restaurar_caliente"), "total_ms"), 50)
        if cal is not None:
            if cal <= 1000:
                veredictos.append("P1: restaurar con caché caliente p50 %s ≤ 1 s → la instancia anidada vale (compárese con metal normalizado)" % fmt(cal))
            elif cal <= 3000:
                veredictos.append("P1: restaurar con caché caliente p50 %s entre 1 y 3 s → idle: pause para el modelo principal" % fmt(cal))
            else:
                veredictos.append("P1: restaurar con caché caliente p50 %s > 3 s → abrir T2-2 o ir a metal" % fmt(cal))
        fria = pr.get("restaurar_fria")
        if not omitida(fria):
            f50 = pct(muestras(fria, "total_ms"), 50)
            if f50 is not None:
                veredictos.append("P1: restaurar tras drop_caches p50 %s %s" % (
                    fmt(f50), "> 5 s → abrir T2-1 (precarga) y recomendar NVMe local" if f50 > 5000 else "≤ 5 s → sin precarga"))

    r = doc.get("restaurar")
    if isinstance(r, dict) and not omitida(r):
        w("restaurar (kling run -from): thaw %s; hasta la primera respuesta %s" % (est(r.get("thaw_ms", [])), est(r.get("hasta_primera_ms", []))))

    t = doc.get("tps")
    if isinstance(t, dict) and not omitida(t):
        w("tok/s en caliente: prompt %s, generación %s" % (fmt(pct(t.get("prompt_tok_s", []), 50), ""), fmt(pct(t.get("generacion_tok_s", []), 50), "")))

    mem = doc.get("memoria")
    if isinstance(mem, dict) and not omitida(mem):
        w("memoria de N réplicas (P2): réplicas, PSS total, PSS por réplica, suma de RSS, Cached del host, disco dormida")
        marg = {}
        for var in ("restauradas", "descongeladas", "remove"):
            pts = mem.get(var)
            if omitida(pts):
                w("  %-14s omitida: %s" % (var, pts["omitida"]))
                continue
            for p in pts or []:
                w("  %-14s %2s  %6s MiB  %-22s rss %-8s cached %-8s disco %s" % (
                    var, p.get("replicas"), p.get("pss_total_mib"), " ".join(str(x) for x in p.get("pss_mib", [])),
                    p.get("rss_total_mib"), (p.get("host") or {}).get("cached_mib"), p.get("disco_dormida_mib", "—")))
            uno, add = marginal(pts or [])
            marg[var] = add
            if add is not None:
                w("  %-14s réplica adicional ≈ %.0f MiB de PSS (%.0f %% del dorado de %s MiB)" % (
                    var, add, 100.0 * add / modelo_mib if modelo_mib else float("nan"), modelo_mib))
        if marg.get("descongeladas") is not None and modelo_mib:
            v1 = marg["descongeladas"] >= 0.5 * modelo_mib
            veredictos.append("P2: una descongelada cuesta %.0f MiB (%.0f %% del modelo) → V1 %s" % (
                marg["descongeladas"], 100.0 * marg["descongeladas"] / modelo_mib,
                "CONFIRMADA" if v1 else "no se confirma (comparte más de lo esperado)"))
        oc = doc.get("ociosa", {})
        g = oc.get("gateway") if isinstance(oc, dict) else None
        rem50 = th50 = None
        if isinstance(g, dict) and not omitida(g):
            rem50, th50 = pct(muestras(g.get("idle_remove"), "ms"), 50), pct(muestras(g.get("idle_freeze"), "ms"), 50)
        elif isinstance(oc, dict) and not omitida(oc):
            rem50, th50 = pct(oc.get("restaurar_ms", []), 50), pct(oc.get("descongelar_ms", []), 50)
        if marg.get("remove") is not None and modelo_mib and rem50 is not None and th50:
            ok = marg["remove"] <= 0.10 * modelo_mib and rem50 <= 1.2 * th50
            veredictos.append("P2: remove cuesta %.0f MiB por réplica y despierta en %s frente a %s del thaw → %s" % (
                marg["remove"], fmt(rem50), fmt(th50), "remove se queda por defecto" if ok else "más lento o más caro: T2-3 (fondo precalentado)"))

    oc = doc.get("ociosa")
    if isinstance(oc, dict) and not omitida(oc):
        w("ociosa → primera respuesta (P2): thaw %s; restaurar %s" % (est(oc.get("descongelar_ms", [])), est(oc.get("restaurar_ms", []))))
        g = oc.get("gateway")
        if isinstance(g, dict) and not omitida(g):
            w("  por el gateway: idle freeze %s; idle remove %s" % (est(muestras(g.get("idle_freeze"), "ms")), est(muestras(g.get("idle_remove"), "ms"))))
            estados = sorted({(d.get("dormidas") or {}).get("estado") for d in g.get("idle_remove") or [] if isinstance(d, dict)} - {None})
            if estados and estados != ["borradas"]:
                w("  AVISO: con idle: remove el segador dejó las réplicas %s (¿falta integrar Z3/Z4?)" % ", ".join(estados))
        elif omitida(g):
            w("  por el gateway: omitida: %s" % g["omitida"])

    s = doc.get("semillas")
    if isinstance(s, dict) and not omitida(s):
        w("semillas: %s" % ("¡la MISMA salida en dos réplicas!" if s.get("iguales") else "distintas en cada réplica"))

    c = doc.get("carga")
    if isinstance(c, dict) and not omitida(c):
        w("carga por %s (P3):" % c.get("ruta"))
        directo = {p.get("clientes"): p for p in ((c.get("directo") or {}).get("puntos") or [])}
        for p in (c.get("gateway") or {}).get("puntos") or []:
            d = directo.get(p.get("clientes"))
            w("  %3s clientes: %7.1f pet/s  p50 %s  p99 %s  errores %s%s" % (
                p.get("clientes"), p.get("rps", 0), fmt(p.get("p50_ms")), fmt(p.get("p99_ms")), p.get("errores"),
                "   directo p50 %s" % fmt(d.get("p50_ms")) if d else ""))
            if p.get("clientes") == 1 and d:
                extra = p.get("p50_ms", 0) - d.get("p50_ms", 0)
                veredictos.append("P3: sobrecoste gateway + microVM con 1 cliente %.2f ms p50 → %s" % (extra, "≤ 1 ms, vale" if extra <= 1 else "> 1 ms"))
            if p.get("clientes") == 16:
                veredictos.append("P3: %s errores con 16 clientes → %s" % (p.get("errores"), "vale" if p.get("errores") == 0 else "NO vale"))
        if m.get("contrato", "").startswith("chispa") and isinstance(pr, dict) and not omitida(pr):
            d50 = pct(muestras(pr.get("restaurar_caliente"), "total_ms"), 50)
            if d50 is not None:
                veredictos.append("P3: primera decisión tras restaurar p50 %s → %s" % (fmt(d50), "≤ 150 ms, vale" if d50 <= 150 else "> 150 ms"))
            if isinstance(mem, dict):
                uno, add = marginal(mem.get("restauradas") or [])
                if uno and add is not None:
                    veredictos.append("P3: réplica adicional %.0f %% del PSS de la primera → %s" % (
                        100.0 * add / uno, "≤ 15 %, vale" if add <= 0.15 * uno else "> 15 % (¿ORT reescribe los pesos al cargar? → C1)"))
    for v in veredictos:
        w("» " + v)


def principal(argv):
    if argv[:1] == ["--self-test"]:
        return autoprueba()
    validar = argv[:1] == ["--validar"]
    fs = argv[1:] if validar else argv
    if not fs:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    rc = 0
    for f in fs:
        try:
            with open(f) as fh:
                doc = json.load(fh)
        except (OSError, ValueError) as e:
            print("%s: %s" % (f, e), file=sys.stderr)
            rc = 1
            continue
        mal = faltan(doc)
        if validar:
            if mal:
                print("%s: faltan %s" % (f, ", ".join(mal)), file=sys.stderr)
                rc = 1
            else:
                print("%s: completo" % f)
            continue
        out = []
        resumen(doc, out)
        if mal:
            out.append("AVISO: faltan claves: %s" % ", ".join(mal))
        print("\n".join(out) + "\n")
    return rc


def autoprueba():
    pt = lambda n, pss, extra=None: dict({"replicas": n, "vivas": n, "pss_total_mib": pss, "pss_mib": [pss // n] * n,
                                          "rss_total_mib": 1900 * n, "host": {"cached_mib": 2000, "available_mib": 9000}}, **(extra or {}))
    sm = lambda ms: {"total_ms": ms, "peticion_ms": ms // 2, "thaw_ms": 20, "resync_ms": 1.5, "load_ms": 9,
                     "fallos": {"minflt": 10, "majflt": 3}, "disco_leido_bytes": 0}
    doc = {
        "esquema": ESQUEMA,
        "modelo": {"snapshot": "von-q8", "modelo": "qwen:q8_0", "contrato": "openai", "vcpus": 4, "mem_mib": 2304,
                   "snapshot_mem_mib": 2000, "backend": "firecracker"},
        "host": {"uname": "Linux", "cpus": 16, "mem_total_mib": 32000, "drop_caches": True, "drop_caches_motivo": None, "secciones": []},
        "frio": {"omitida": "no pedida"},
        "restaurar": {"thaw_ms": [20, 22], "hasta_primera_ms": [400, 420], "peticion_ms": [300, 310]},
        "primera": {"restaurar_caliente": [sm(400), sm(500), sm(450)], "restaurar_fria": [sm(6000)],
                    "descongelar_caliente": [sm(380)], "descongelar_fria": {"omitida": "x"}},
        "tps": {"prompt_tok_s": [100], "generacion_tok_s": [10.5]},
        "memoria": {"restauradas": [pt(1, 2100), pt(2, 2150)],
                    "descongeladas": [pt(1, 2100, {"disco_dormida_mib": 1900}), pt(2, 4100, {"disco_dormida_mib": 1900})],
                    "remove": [pt(1, 2100), pt(2, 2140)]},
        "ociosa": {"descongelar_ms": [400, 410], "restaurar_ms": [420, 430],
                   "gateway": {"idle_freeze": [{"ms": 400}], "idle_remove": [{"ms": 440, "dormidas": {"estado": "borradas"}}]}},
        "semillas": {"iguales": False},
        "carga": {"ruta": "/v1/m/von-q8/v1/chat/completions",
                  "gateway": {"puntos": [{"clientes": 1, "rps": 10, "p50_ms": 50.5, "p99_ms": 60, "errores": 0},
                                         {"clientes": 16, "rps": 90, "p50_ms": 150, "p99_ms": 300, "errores": 0}]},
                  "directo": {"puntos": [{"clientes": 1, "p50_ms": 50}]}},
    }
    assert faltan(doc) == [], faltan(doc)
    roto = json.loads(json.dumps(doc))
    del roto["memoria"]["remove"]
    del roto["carga"]
    assert faltan(roto) == ["memoria.remove", "carga"], faltan(roto)
    assert pct([5, 1, 4, 2, 3, 10, 9, 8, 7, 6], 50) == 5 and pct([5, 1, 4, 2, 3, 10, 9, 8, 7, 6], 95) == 10
    assert pct([], 50) is None
    out = []
    resumen(doc, out)
    txt = "\n".join(out)
    for want in ("V1 CONFIRMADA", "remove se queda por defecto", "≤ 1 s → la instancia anidada vale",
                 "> 5 s → abrir T2-1", "0.50 ms p50 → ≤ 1 ms, vale", "0 errores con 16 clientes → vale"):
        assert want in txt, (want, txt)

    # C9-14: una muestra «caliente» que leyó del disco es fría; la corrida se rechaza.
    assert rechazos(doc) == [], rechazos(doc)
    fria = json.loads(json.dumps(doc))
    fria["primera"]["restaurar_caliente"][1]["disco_leido_bytes"] = 1085689856
    r = rechazos(fria)
    assert len(r) == 1 and "restaurar_caliente" in r[0] and "1085689856" in r[0], r
    tolerada = json.loads(json.dumps(fria))
    tolerada["primera"]["umbral_caliente_bytes"] = 2 << 30
    assert rechazos(tolerada) == [], rechazos(tolerada)
    vacia = json.loads(json.dumps(doc))
    vacia["primera"]["descongelar_caliente"] = []
    vacia["primera"]["calientes_con_disco"] = {"descongelar_caliente": [sm(7900)]}
    assert any("descongelar_caliente" in x for x in rechazos(vacia)), rechazos(vacia)

    # C9-12: sin journal del daemon (o sin desglose / service.log de un dorado) no hay corrida.
    import os
    import tempfile
    with tempfile.TemporaryDirectory() as base:
        assert any("corrida.json" in x for x in rechazos_corrida(None, base)), "sin corrida.json"
        with open(os.path.join(base, "journal-kt-bench.log"), "w") as fh:
            fh.write("nov 23 10:00:00 kling[1]: commit von-q4-golden-x -> von-q4: overlay 80 ms (total 900 ms)\n")
        os.mkdir(os.path.join(base, "dorado-von-q4"))
        for nom in ("service.log", "meminfo.jsonl"):
            with open(os.path.join(base, "dorado-von-q4", nom), "w") as fh:
                fh.write("x\n")
        cor = {"esquema": ESQUEMA_CORRIDA, "journal": {"unidad": "kt-bench", "fichero": "journal-kt-bench.log"},
               "dorados": [{"dorado": "von-q4", "rc": 0, "fabricar_ms": 40000, "commit": {"overlay": 80, "total": 900},
                            "service_log": "dorado-von-q4/service.log", "meminfo": "dorado-von-q4/meminfo.jsonl"},
                           {"dorado": "von-q8", "rc": 1, "fabricar_ms": 300000, "commit": None,
                            "service_log": "dorado-von-q4/service.log", "meminfo": "dorado-von-q4/meminfo.jsonl"}]}
        assert rechazos_corrida(cor, base) == [], rechazos_corrida(cor, base)
        sinj = json.loads(json.dumps(cor))
        sinj["journal"] = {"omitido": "sin systemd"}
        assert any("journal" in x for x in rechazos_corrida(sinj, base)), rechazos_corrida(sinj, base)
        os.remove(os.path.join(base, "journal-kt-bench.log"))
        assert any("journal" in x for x in rechazos_corrida(cor, base)), "journal borrado"
        with open(os.path.join(base, "journal-kt-bench.log"), "w") as fh:
            fh.write("x\n")
        sind = json.loads(json.dumps(cor))
        sind["dorados"][0]["commit"] = None
        assert any("desglose" in x and "von-q4" in x for x in rechazos_corrida(sind, base)), rechazos_corrida(sind, base)
        sins = json.loads(json.dumps(cor))
        sins["dorados"][1]["service_log"] = None
        assert any("service.log" in x and "von-q8" in x for x in rechazos_corrida(sins, base)), rechazos_corrida(sins, base)
        txt = "\n".join(resumen_corrida(cor, base))
        assert "von-q4" in txt and "overlay 80" in txt, txt
    print("ia.py: autoprueba OK")
    return 0


if __name__ == "__main__":
    sys.exit(principal(sys.argv[1:]))
