# API /v1 del gateway de sandboxes

La referencia completa es la spec OpenAPI 3.1: [`ext/sandbox/api/openapi.yaml`](../ext/sandbox/api/openapi.yaml).
Un test de contrato (`ext/sandbox/internal/frontal/contrato_test.go`) falla si una ruta
del código falta en la spec o sobra, o si un esquema deja de coincidir con los
`json` tags de su struct.

## Autenticación

Todas las rutas salvo `GET /v1/health` piden `Authorization: Bearer <token>`.
El token es el de un inquilino (sus sandboxes, grafos, plantillas y métricas) o el de
un admin (además `GET /v1/hosts` y las métricas de todos). Sin token o con uno
inválido, 401 con `WWW-Authenticate: Bearer realm="kindling-sandbox"`.

Los errores son JSON `{"message": "...", "code": "..."}`; el código HTTP manda y `code`
puede faltar.

El id de un sandbox es `host/máquina`: la barra va sin codificar en la ruta
(`/v1/sandboxes/h1/m-abc/exec`).

## Streaming de exec

`POST /v1/sandboxes/{id}/exec` responde `application/x-ndjson`: una línea JSON por
evento, vaciada al momento (`{"stream":"stdout","data":"<base64>"}`), y al final
`{"exit":0,"duration_ms":...}`, o `{"error":"..."}` si el flujo se corta después del 200.
Con `?wait=1` espera y devuelve un único `ExecResult` JSON.

## Shell interactiva

`POST /v1/sandboxes/{id}/shell` con `Upgrade: kling-shell/1` cambia el protocolo
(`101`) a tramas binarias `tipo(1) | longitud(uint32 BE) | carga`. La spec documenta los
tipos de trama.

## Ficheros

`/v1/sandboxes/{id}/files?path=...`: GET descarga (o `stat=1` da metadatos), PUT sube
(`mode` octal, `mkdir=1`, máximo 64 MiB) y DELETE borra.
