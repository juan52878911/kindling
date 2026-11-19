# Prueba real de la paridad

Tres pruebas contra microVMs de verdad, para lo que los tests con daemons
falsos no pueden ver. Úsalas con un daemon de prueba (el privado de
`scripts/93-e2e-linux-ci.sh` o uno equivalente), nunca con uno que importe.

| Script | Qué prueba |
|---|---|
| `nucleo.sh` | `exec -d` y `kling proc`, sesiones, procesos que sobreviven a freeze/thaw, `kling ls` y `cp` de directorios |
| `egress.sh` | `kling egress set` en caliente: none, allowlist e internet, en todos los sentidos (necesita salida a internet) |
| `gateway.py` | `kling sbx gateway`: SDKs de Python y TS, entrada `<puerto>-<id>.<dominio>` (HTTP y WebSocket), consola, inquilinos y claves, MCP, SDKs reales de E2B por `kling sbx e2b`, auditoría y OTLP |

Necesitan la imagen `toolchain` (`kling image toolchain`) en el daemon y
`KLING_BIN` (o `PARIDAD_HOME/bins`) con `kling` y `kling-sandbox` de este commit.

La primera pasada real (2026-11-19, kling-pve, Firecracker 1.17) encontró tres
fallos que los tests no veían: la entrada daba 502 porque el gateway no
declaraba el puerto en `kling.ports`, el shim de E2B lanzaba `/bin/bash` en
imágenes sin bash, y volver a allowlist en caliente dejaba el DNS detrás del
DROP de FORWARD. Los tres están arreglados; con ellos pasan las 25 pruebas del
gateway, las del núcleo y todas las transiciones de egress.
