"""code-server dentro de un sandbox, publicado tras un endpoint del gateway.

Crea un sandbox desde la plantilla code-server, arranca code-server como proceso de fondo
(sigue vivo aunque este script termine), publica el puerto 8080 y escribe la URL.
No llama a ningún LLM.
"""
from kindling_sandbox import Sandbox

PUERTO = 8080

sb = Sandbox.create(template="code-server", ttl=3600)
try:
    # --auth none: el acceso lo protege el endpoint (su token), no code-server.
    sb.processes.start(
        ["code-server", "--bind-addr", f"0.0.0.0:{PUERTO}", "--auth", "none", "/home"])
    ep = sb.endpoints.create(PUERTO, ttl=3600)
    print("sandbox:", sb.id)
    print("endpoint:", ep)
    print(f"Open the endpoint in a browser; `kling sbx rm {sb.id}` removes it all.")
except Exception:
    sb.delete()
    raise
