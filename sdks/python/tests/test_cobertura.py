"""Cada operación de ext/sandbox/api/openapi.yaml tiene un método en el SDK o figura
en EXCLUIDAS. Si no, el test falla: la spec y el SDK no pueden derivar en silencio.

También genera la tabla de cobertura del README (entre las marcas `coverage`):
`KLING_UPDATE_README=1 python3 -m unittest discover sdks/python/tests` la reescribe,
y sin esa variable el test falla si el README no la tiene al día.
"""

import os
import re
import sys
import unittest

AQUI = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(AQUI, ".."))

import kindling_sandbox as k  # noqa: E402

SPEC = os.path.join(AQUI, "..", "..", "..", "ext", "sandbox", "api", "openapi.yaml")
README = os.path.join(AQUI, "..", "README.md")

# Operaciones que el SDK no cubre a propósito, con el motivo.
EXCLUIDAS = {
    "POST /v1/sandboxes/{id}/shell": "interactive shell: upgrade to binary frames (kling-shell/1), out of scope",
}

# operación -> "Clase.método" del SDK de Python.
METODOS = {
    "GET /v1/health": "Client.health",
    "GET /v1/hosts": "Client.hosts",
    "GET /v1/metrics": "Client.metrics",
    "GET /v1/templates": "Client.templates",
    "GET /v1/sandboxes": "Sandbox.list",
    "POST /v1/sandboxes": "Sandbox.create",
    "GET /v1/sandboxes/{id}": "Sandbox.connect",
    "DELETE /v1/sandboxes/{id}": "Sandbox.delete",
    "POST /v1/sandboxes/{id}/renew": "Sandbox.renew",
    "POST /v1/sandboxes/{id}/pause": "Sandbox.pause",
    "POST /v1/sandboxes/{id}/resume": "Sandbox.resume",
    "PATCH /v1/sandboxes/{id}/egress": "Sandbox.set_egress",
    "POST /v1/sandboxes/{id}/endpoints": "Endpoints.create",
    "GET /v1/sandboxes/{id}/endpoints": "Endpoints.list",
    "DELETE /v1/sandboxes/{id}/endpoints/{port}": "Endpoints.revoke",
    "POST /v1/sandboxes/{id}/exec": "Sandbox.exec",
    "POST /v1/sandboxes/{id}/code": "Code.run",
    "POST /v1/sandboxes/{id}/code/contexts": "CodeContexts.create",
    "GET /v1/sandboxes/{id}/code/contexts": "CodeContexts.list",
    "GET /v1/sandboxes/{id}/code/contexts/{cid}": "CodeContexts.get",
    "DELETE /v1/sandboxes/{id}/code/contexts/{cid}": "CodeContexts.delete",
    "DELETE /v1/sandboxes/{id}/code/contexts/{cid}/execution": "Code.interrupt",
    "POST /v1/sandboxes/{id}/processes": "Processes.start",
    "GET /v1/sandboxes/{id}/processes": "Processes.list",
    "GET /v1/sandboxes/{id}/processes/{pid}": "Processes.get",
    "DELETE /v1/sandboxes/{id}/processes/{pid}": "Processes.delete",
    "POST /v1/sandboxes/{id}/processes/{pid}/signal": "Processes.signal",
    "GET /v1/sandboxes/{id}/processes/{pid}/output": "Processes.output",
    "POST /v1/sandboxes/{id}/sessions": "Sessions.create",
    "GET /v1/sandboxes/{id}/sessions": "Sessions.list",
    "GET /v1/sandboxes/{id}/sessions/{sid}": "Sessions.get",
    "DELETE /v1/sandboxes/{id}/sessions/{sid}": "Sessions.delete",
    "GET /v1/sandboxes/{id}/files": "Files.read",
    "PUT /v1/sandboxes/{id}/files": "Files.write",
    "DELETE /v1/sandboxes/{id}/files": "Files.delete",
    "POST /v1/sandboxes/{id}/files/move": "Files.move",
    "POST /v1/sandboxes/{id}/files/mkdir": "Files.mkdir",
    "POST /v1/sandboxes/{id}/files/chmod": "Files.chmod",
    "POST /v1/sandboxes/{id}/files/replace": "Files.replace",
    "PATCH /v1/sandboxes/{id}/metadata": "Sandbox.set_metadata",
    "POST /v1/sandboxes/{id}/fork": "Sandbox.fork",
    "GET /v1/sandboxes/{id}/metrics": "Sandbox.metrics",
    "GET /v1/sandboxes/{id}/files/search": "Files.search",
    "GET /v1/volumes": "Client.list_volumes",
    "POST /v1/volumes": "Client.create_volume",
    "GET /v1/volumes/{id}": "Client.get_volume",
    "DELETE /v1/volumes/{id}": "Client.delete_volume",
    "GET /v1/graphs": "Client.list_graphs",
    "POST /v1/graphs": "Client.create_graph",
    "GET /v1/graphs/{id}": "Client.get_graph",
    "DELETE /v1/graphs/{id}": "Client.delete_graph",
    "GET /v1/tenants": "Admin.list_tenants",
    "POST /v1/tenants": "Admin.create_tenant",
    "GET /v1/tenants/{id}": "Admin.get_tenant",
    "PATCH /v1/tenants/{id}": "Admin.update_tenant",
    "DELETE /v1/tenants/{id}": "Admin.delete_tenant",
    "POST /v1/tenants/{id}/keys": "Admin.create_key",
    "POST /v1/tenants/{id}/keys/{kid}/rotate": "Admin.rotate_key",
    "DELETE /v1/tenants/{id}/keys/{kid}": "Admin.revoke_key",
}


def operaciones():
    """Operaciones de la spec, en orden, con un parser mínimo (sin dependencias)."""
    with open(SPEC, encoding="utf-8") as f:
        texto = f.read()
    paths = texto.split("\npaths:")[1].split("\ncomponents:")[0]
    ruta, ops = None, []
    for linea in paths.split("\n"):
        m = re.match(r"^  (/\S+):\s*$", linea)
        if m:
            ruta = m.group(1)
            continue
        m = re.match(r"^    (get|post|put|delete|patch):\s*$", linea)
        if m and ruta:
            ops.append("%s %s" % (m.group(1).upper(), ruta))
    return ops


def tabla():
    filas = ["| Operation | SDK |", "|---|---|"]
    for op in operaciones():
        if op in EXCLUIDAS:
            sdk = "not covered: " + EXCLUIDAS[op]
        else:
            clase, _, metodo = METODOS[op].partition(".")
            sdk = "`%s.%s()`" % (clase, metodo)
        filas.append("| `%s` | %s |" % (op, sdk))
    return "\n".join(filas)


class TestCobertura(unittest.TestCase):
    def test_toda_operacion_tiene_metodo_o_exclusion(self):
        ops = operaciones()
        self.assertGreater(len(ops), 40, "el parser no leyó la spec")
        sin = [o for o in ops if o not in METODOS and o not in EXCLUIDAS]
        self.assertEqual(sin, [], "operaciones de la spec sin método en el SDK ni exclusión: %s" % sin)
        viejas = [o for o in list(METODOS) + list(EXCLUIDAS) if o not in ops]
        self.assertEqual(viejas, [], "entradas del test que ya no están en la spec: %s" % viejas)

    def test_los_metodos_existen(self):
        for op, ref in METODOS.items():
            clase, _, metodo = ref.partition(".")
            self.assertTrue(hasattr(getattr(k, clase), metodo), "%s: falta %s" % (op, ref))

    def test_readme_tiene_la_tabla_al_dia(self):
        with open(README, encoding="utf-8") as f:
            readme = f.read()
        patron = re.compile(r"(<!-- coverage:begin -->\n)(.*?)(\n<!-- coverage:end -->)", re.S)
        self.assertRegex(readme, patron, "faltan las marcas coverage en el README")
        if os.environ.get("KLING_UPDATE_README") == "1":
            nuevo = patron.sub(lambda m: m.group(1) + tabla() + m.group(3), readme)
            with open(README, "w", encoding="utf-8") as f:
                f.write(nuevo)
            return
        self.assertEqual(patron.search(readme).group(2), tabla(),
                         "tabla vieja: KLING_UPDATE_README=1 python3 -m unittest discover sdks/python/tests")


if __name__ == "__main__":
    unittest.main()
