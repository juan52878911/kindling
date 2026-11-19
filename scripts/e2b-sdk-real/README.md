# Prueba del shim E2B con los SDKs reales

Prueba manual (necesita red para instalar los SDKs): el SDK de E2B habla con `kling sbx e2b`,
y el shim con el gateway falso de `sdks/python/tests/servidor_falso.py`. Cubre create,
`commands.run`, `files.write`/`read`, list y kill sin DNS ni TLS gracias a `E2B_SANDBOX_URL`.

```bash
CGO_ENABLED=0 go build -o scripts/e2b-sdk-real/kling-sandbox ./ext/sandbox/cmd/kling-sandbox
python3 -m venv /tmp/e2b-venv && /tmp/e2b-venv/bin/pip install e2b
/tmp/e2b-venv/bin/python scripts/e2b-sdk-real/e2b_real.py          # SDK de Python
(cd scripts/e2b-sdk-real/js && npm i e2b@2) && /tmp/e2b-venv/bin/python scripts/e2b-sdk-real/e2b_js.py   # SDK de JS
```

`commands.run` termina con código 3 y «err» a propósito: es lo que devuelve el gateway falso,
y el SDK lo convierte en `CommandExitException`. El arnés hace que el falso decodifique `%2F`
en la ruta, hable HTTP/1.1 y lea cuerpos en trozos, como el frontal real en Go.
Probado el 2026-11-19 con e2b 2.64.0 (Python) y 2.55.0 (JS).
