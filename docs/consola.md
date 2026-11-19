# Consola web del gateway

El gateway sirve una consola mínima en `/console/` (HTML, JS y CSS embebidos con `embed`;
sin dependencias ni CDN). Es un cliente más de la API `/v1`: no amplía permisos.

## Abrirla

Abre `https://<gateway>/console/` en el navegador y pega tu token de inquilino. El token
vive solo en `sessionStorage` de la pestaña: nunca va en la URL ni en cookies. «Salir» lo borra.

Cabeceras: `Content-Security-Policy: default-src 'self' … frame-ancestors 'none'`,
`X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`.
La salida del invitado se pinta siempre como texto (`textContent`); un test falla si el JS
usa `innerHTML`, `eval` o `document.write`, y otro si el HTML carga algo de fuera.

## Vistas (captura en texto)

```
kindling sandbox   Sandboxes  Plantillas                         [Salir]
--------------------------------------------------------------------------
Sandboxes
 id           estado   plantilla  expira
 lab/ab12     running  python     2026-11-19T10:00Z      [Borrar]
 Crear desde plantilla: [plantilla] [ttl (s)] [Crear]

Sandbox lab/ab12
 { JSON del sandbox }                                    [Renovar]
 Exec:      [comando (sh -c)] [Ejecutar]   salida en flujo NDJSON, stderr en rojo, [exit N]
 Procesos y logs: [Listar procesos] [pid] [Ver logs]
 Ficheros:  [/ruta] [Listar] [Ver/bajar] [elegir fichero] [Subir]
 Entrada:   [puerto] [Crear endpoint]

Plantillas
 { catálogo de /v1/templates }
```

«Listar» usa `GET /v1/sandboxes/{id}/files?list=1&path=…` (tipo, modo, tamaño y nombre de cada entrada;
avisa si el listado se truncó).

## Bandera `-console`

`kling sbx gateway -console[=true|false]` decide si se sirve `/console/` (apagada: 404).
Por defecto está **encendida si el gateway escucha con TLS o en loopback** y **apagada si escucha en
HTTP plano fuera de loopback** (`-insecure-listen`), porque la consola maneja tokens de inquilino. Una
bandera explícita manda sobre el valor por defecto. En el chart: `gateway.console` (`""` = valor por
defecto, `"true"` / `"false"` lo fuerzan); ojo, con `gateway.insecureListen` el defecto es apagada.
