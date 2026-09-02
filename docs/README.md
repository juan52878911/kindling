# Documentación de kindling

Punto de entrada: el [README](../README.md) ([English](../README.md) ·
[Español](../README.es.md)). Lo que hay aquí es lo que no cabe en él: recetas
reproducibles, auditorías con números, y las notas de campo que evitan repetir horas de
depuración.

## Guías

| Documento | Qué cubre | Léelo si… |
|---|---|---|
| [`mac-arm64.md`](mac-arm64.md) | kindling en Apple Silicon: la VM Lima con virtualización anidada, los límites honestos (M3+, ~16 s de arranque en frío) y las tres palancas medidas para bajarlo a ~2,5 s (`-bundle`, `-cpu-pct 100`, http-proxy) | vas a correrlo en un Mac |
| [`releases.md`](releases.md) | Cómo se compilan, verifican y publican los binarios; cómo crear una release nueva | mantienes el proyecto o quieres compilar desde fuentes |
| [`densidad-zram.md`](densidad-zram.md) | Swap comprimido en RAM para densificar el host: cuándo ayuda, cuándo estorba, y el plan de medición antes/después | quieres más microVMs co-residentes sin más RAM |

## Diseño y auditorías

| Documento | Qué cubre | Léelo si… |
|---|---|---|
| [`three-layers.md`](three-layers.md) | Imágenes por capas (base por familia de runtime + capa de servicio + overlay): diseño, decisiones, y el parque real reimportado — 1300 → 433 MiB | quieres entender `-base`, las familias `node`/`python` o el camino PyPI |
| [`estabilidad.md`](estabilidad.md) | La auditoría de estabilidad y determinismo sobre el sistema vivo: el commit prematuro, los seis fallos de robustez, el hasheo que costaba el 67% del despertar, y la prueba de estrés de 142 microVMs | quieres saber por qué v0.4 es 9,5× más rápida bajo carga, o cómo se depuró |
| [`hallazgos.md`](hallazgos.md) | Notas de campo acumuladas: overlays, ficheros dispersos, namespaces, cgroups, trampas de medición… cada una con su porqué | algo se comporta raro y sospechas que ya le pasó a alguien |

## Notas de release

| | |
|---|---|
| [`../CHANGELOG.md`](../CHANGELOG.md) | Todas las versiones, resumidas |
| [`RELEASE-v0.3.0.md`](RELEASE-v0.3.0.md) | v0.3.0 con tablas comparativas: réplicas, MMDS, allowlist, Mac arm64 |
| [`RELEASE-v0.2.0.md`](RELEASE-v0.2.0.md) | v0.2.0 con tablas comparativas: `kling up`, token, volúmenes |

## Seguridad

El modelo de amenaza, las barreras y — sobre todo — lo que NO está resuelto:
[`../SECURITY.md`](../SECURITY.md).
