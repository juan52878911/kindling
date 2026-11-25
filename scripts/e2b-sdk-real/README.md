# Prueba del shim E2B con los SDK reales

`e2b_real.py` (SDK de Python, `e2b==2.64.0`) y `e2b_real.mjs` (SDK de JS, `e2b@2.64.0`, las
versiones que fija `docs/e2b.md`) usan el SDK oficial de E2B contra `kling sbx e2b`: crear,
`commands.run` (salida y `on_stdout`, exit 3 con `CommandExitException`, `envs` y `cwd`), fondo
con `wait`, `list` + `kill` y `connect`, ficheros (write/read en texto y bytes, exists,
get_info, make_dir, rename, list, remove, varios a la vez), `Sandbox.list` por metadata,
`get_info`, `is_running`, `set_timeout`, `Sandbox.connect`, `update_network`, pausar y reanudar
con `connect`, que `send_stdin`/`watch_dir`/`pty` fallen con error (no se cuelguen) y borrar.
Imprimen una línea por paso y terminan con `E2B OK n/n` (código 0) o `E2B FALLA` (código 1);
sin el paquete, código 3.

El SDK se apunta al shim con `E2B_API_URL` y `E2B_SANDBOX_URL` (el modo de pruebas locales de
`docs/e2b.md`: sin DNS ni TLS). `e2b_real.py --debug <dir>` repite los pasos de envd con
`E2B_DEBUG=true` (envd en `localhost:49983`, sin plano de control).

**Sin VM (lo que corre en CI).** `TestContratoE2B` (`ext/sandbox/internal/frontal`) monta el
shim delante del frontal real con su daemon falso, que ejecuta las órdenes, los procesos de
fondo y los ficheros en la máquina del test, y corre los dos scripts (y el de Python en modo
debug, si `127.0.0.1:49983` está libre):

```bash
python3 -m venv /tmp/e2b-venv && /tmp/e2b-venv/bin/pip install 'e2b==2.64.0'
mkdir -p /tmp/e2b-js && npm install --prefix /tmp/e2b-js --no-save 'e2b@2.64.0'
cd ext/sandbox && KLING_E2B_PYTHON=/tmp/e2b-venv/bin/python KLING_E2B_NODE=/tmp/e2b-js \
  go test -run TestContratoE2B -v ./internal/frontal/
```

Sin `KLING_E2B_PYTHON` se prueba `python3`; sin `KLING_E2B_NODE`, el subtest de JS se salta.
Con `KLING_E2B_REQUIRED=1` (CI), que falte un SDK es un fallo.

**Contra un gateway vivo (el lab).** Con una plantilla construida (aquí `base`):

```bash
kling sbx e2b -listen 127.0.0.1:3000 &
/tmp/e2b-venv/bin/python scripts/e2b-sdk-real/e2b_real.py http://127.0.0.1:3000 "$KLING_SANDBOX_TOKEN" /tmp base
KLING_E2B_NODE=/tmp/e2b-js node scripts/e2b-sdk-real/e2b_real.mjs http://127.0.0.1:3000 "$KLING_SANDBOX_TOKEN" /tmp base
```

El tercer argumento es el directorio del sandbox donde se escriben los ficheros de prueba. El
paso de red cambia el `egress` del sandbox a `allowlist` con `pypi.org` y luego lo abre.
