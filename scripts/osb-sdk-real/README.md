# Prueba del shim OpenSandbox con el SDK real

`osb_real.py` usa el SDK oficial de OpenSandbox para Python (`opensandbox==1.2.1`, la versión
que fija `docs/opensandbox.md`) contra `kling sbx osb`: crear, `commands.run` (salida 0 y 3),
escribir y leer, info/replace/mv/search/list/delete de ficheros, comando de fondo con estado y
logs, sesión, listado, política de red, renovar, pausar, reanudar y borrar. Imprime una línea
por paso y termina con `OSB OK n/n` (código 0) o `OSB FALLA` (código 1); sin el paquete, código 3.

**Sin VM (lo que corre en CI).** `TestContratoOSB` (`ext/sandbox/internal/frontal`) monta el
shim delante del frontal real con su daemon falso, que ejecuta las órdenes y los ficheros en la
máquina del test, y corre este script:

```bash
python3 -m venv /tmp/osb-venv && /tmp/osb-venv/bin/pip install 'opensandbox==1.2.1'
cd ext/sandbox && KLING_OSB_PYTHON=/tmp/osb-venv/bin/python go test -run TestContratoOSB -v ./internal/frontal/
```

**Contra un gateway vivo (el lab).** Con una plantilla con Python construida (aquí `py`):

```bash
kling sbx osb -listen 127.0.0.1:8080 -gateway https://gateway:8090 -image-map python:3.12=py &
/tmp/osb-venv/bin/python scripts/osb-sdk-real/osb_real.py http://127.0.0.1:8080 "$KLING_SANDBOX_TOKEN" /tmp python:3.12
```

El tercer argumento es el directorio del sandbox donde se escriben los ficheros de prueba. El
paso de política de red cambia el `egress` del sandbox a `allowlist` con `pypi.org`.
