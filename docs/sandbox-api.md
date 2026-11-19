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

## Entrada HTTP a puertos del sandbox

Con `kling sbx gateway -ingress-domain sbx.example.com` el gateway sirve los puertos de un
sandbox en `<puerto>-<id>.sbx.example.com`, con HTTP y WebSocket. `<id>` es el id del
gateway (`host/máquina`) en base32 minúsculas sin relleno, para que quepa en un nombre DNS
(la etiqueta entera, puerto incluido, no puede pasar de 63 caracteres; si no cabe, la
creación del endpoint da 422).

**DNS y TLS.** Hace falta un registro comodín `*.sbx.example.com` que apunte al gateway y,
si se sirve HTTPS (lo normal: `-tls-cert`/`-tls-key`), un certificado comodín para ese
dominio. Los nombres son uno por (puerto, sandbox), así que no hay forma de evitar el
comodín. Tras un balanceador que ponga el TLS, `-ingress-domain` sigue valiendo: las URL
que devuelve la API usan `https` si el gateway tiene certificado y `http` si no.

**Abrir un puerto.** `POST /v1/sandboxes/{id}/endpoints` con `{"port": 3000,
"ttl_seconds": 3600, "public": false}` (TTL por defecto 1 h, máximo 7 días) devuelve la `url`
y un `token`. El token no tiene estado (HMAC con `-ingress-secret-file`; sin él es uno al
azar por proceso y no sobrevive a un reinicio ni vale en otro gateway): no se puede revocar
antes de que caduque, solo acortar su vida al pedirlo. Con `"public": true` no hay token:
cualquiera con la URL entra hasta que caduque.

**Autorización.** En el subdominio vale el Bearer del tenant dueño, o el token del endpoint
(solo para ese sandbox y ese puerto) por `Authorization: Bearer`, `X-Kling-Endpoint-Token`
o `?kling_token=`. Este último, pensado para abrir la URL en un navegador, se cambia por
una cookie `kling_ep` HttpOnly del subdominio y una redirección 303 sin el parámetro. El
token del tenant solo se acepta por cabecera. Sin credencial, 401 (igual para un sandbox
que no existe); credencial válida de quien no es dueño, 404. El invitado nunca recibe
`Authorization`, el token de endpoint ni la cookie `kling_ep` (las demás cookies pasan).

**Transporte.** Contra un host local al gateway (socket Unix) se publica el puerto del
invitado en el daemon (`api.Publish`) la primera vez y se hace de proxy inverso a ese
puerto: cuerpos binarios, SSE y WebSocket. Contra un host `ssh://` el `127.0.0.1` publicado
sería el del otro extremo, y solo queda `POST /machines/{ref}/guest` del daemon: HTTP de
texto, cuerpos de hasta 1 MiB, el puerto tiene que estar en la etiqueta `kling.ports` de la
máquina y un WebSocket da 501. Un sandbox congelado se despierta (`thaw`) en el primer
acceso. Si nada escucha en el puerto, 502 con el puerto en el mensaje.

**Topes.** Cabeceras de la petición: 32 KiB (`-ingress-max-header-bytes`, 431); conexiones
simultáneas por sandbox, WebSocket incluidos: 64 (`-ingress-max-conns`, 429); espera a las
cabeceras de la respuesta del invitado: 60 s (504). El acceso por la URL no renueva el TTL
del sandbox: ninguna otra ruta del gateway lo hace por actividad.
