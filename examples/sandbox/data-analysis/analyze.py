"""Análisis de datos con el intérprete de código del sandbox (/code).

Sube un CSV inventado, abre un contexto de Python (guarda el estado entre llamadas) y hace
tres pasos: cargar, agregar, calcular. No llama a ningún LLM. Solo biblioteca estándar, así
que funciona con la plantilla `python` tal cual.
"""
from kindling_sandbox import Sandbox

VENTAS = """region,producto,unidades,precio
norte,tornillo,120,0.10
norte,tuerca,80,0.15
sur,tornillo,200,0.10
sur,arandela,50,0.05
este,tuerca,30,0.15
este,tornillo,75,0.10
"""

with Sandbox.create(template="python", ttl=600) as sb:  # se borra al salir
    sb.files.write("/tmp/ventas.csv", VENTAS, mkdir=True)
    ctx = sb.code.contexts.create(language="python")

    # Cada llamada ve las variables de la anterior: es un contexto, no un proceso nuevo.
    pasos = [
        "import csv\nfilas = list(csv.DictReader(open('/tmp/ventas.csv')))\nlen(filas)",
        "from collections import defaultdict\n"
        "por_region = defaultdict(float)\n"
        "for f in filas:\n"
        "    por_region[f['region']] += int(f['unidades']) * float(f['precio'])\n"
        "dict(por_region)",
        "max(por_region, key=por_region.get)",
    ]
    for codigo in pasos:
        r = sb.code.run_wait(codigo, context=ctx.id)
        if r.error:
            raise SystemExit(f"error en el sandbox: {r.error}")
        print(r.results.get("text/plain"))
