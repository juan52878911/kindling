# Base de datos desechable por test, con el almacén de copia al escribir — 2026-09-29

Repetición del banco de `../db-fork-20260928/` (issue #76) con el almacén Btrfs del núcleo
activo (`daemon.cow=auto`, que elige el almacén con reflink en `/var/lib/kindling/cow`).
Mismo host (CT 105: Proxmox LXC, i7-8700T, 4 CPU, 8 GiB) y mismo seed: tabla `items` de
400 000 filas (70 MB). Daemon: `kling v0.16.0-222-g6fae27c` (pila media A, PR #100). El golden `benchs` está configurado a 192 MiB por copia. La
latencia va desde que se suelta la ráfaga hasta que el primer `count(*)` devuelve 400 000.
Se midieron 3 rondas por celda y no se repitió ni se descartó ninguna.

| N copias | antes (ext4, copia completa del overlay) | ahora (reflink en el almacén Btrfs) | disco por copia |
|---:|---|---|---:|
| 1 | 191 ms (p50; 1.ª en frío 1,7 s) | **133 ms** (p50; 1.ª en frío 2,0 s) | 211 → **3,6 MiB** |
| 8 | 1 032 – 1 252 ms | **341 – 352 ms** | 211 → **3,6 MiB** |
| 32 | 6,0 s → 9,9 s → 15,0 s (sube en cada ronda) | **1,17 s → 1,32 s → 1,27 s** (96/96, plano) | 211 → **3,6 MiB** |

El cuello de botella del banco anterior era escribir 32 overlays enteros (~6,7 GiB) en
cada ronda. Con reflink la copia solo cuesta lo que la copia escribe, y la latencia deja de
crecer de ronda en ronda. El almacén pasó de 1 697 a 1 307 MiB libres tras las 131 copias
del banco.

Notas honestas:

- Con 32 copias, el banco se ejecutó con `-est-disk-mib 16`. Su regla de disco por defecto
  supone 128 MiB por copia y saltaba la celda; se midieron 3,6 MiB por copia.
- La primera copia en frío sigue en ~2 s: incluye cargar el golden en la caché de páginas.
- 32 copias × 192 MiB configurados caben por la admisión de memoria; la RAM real usada en
  el pico fue de 554 MiB (~17 MiB por copia).

Datos crudos: `kindling-cow-n1-8/` (N = 1 y 8) y `kindling-cow-32/` (N = 32).
