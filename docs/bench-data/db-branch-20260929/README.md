# `kling db branch`: lo que tarda un `git checkout` — 2026-09-29

Banco de `scripts/bench-db-branch.sh`: un repo git temporal, `kling db branch -golden
<golden>` en `main`, el gancho `post-checkout` instalado y N checkouts por caso,
cronometrados de extremo a extremo (el reloj envuelve el `git checkout` entero, gancho
incluido). Entre medida y medida se espera a que acabe el trabajo de segundo plano del
gancho. No se descartó ni se repitió ninguna medida.

| caso | qué mide |
|---|---|
| `existing` | volver a una rama que ya tiene copia (`main` ↔ `other`) |
| `new-spare` | `git checkout -b` desde `main` con la copia de reserva lista |
| `new-fork` | `git checkout -b` con las reservas apagadas (`KLING_DB_BRANCH_SPARE=0`): fork en caliente de la copia de `main` |

## Resultados (ms)

| | antes (main, 9a7118a) | ahora p50 | ahora p95 | n |
|---|---:|---:|---:|---:|
| **Linux** `existing` | 1 985 | **43** | 70 | 20 |
| **Linux** `new-spare` | 6 719 | **88** | 118 | 20 |
| **Linux** `new-fork` (uno tras otro) | 6 719 | 4 526 | 5 664 | 20 |
| **Linux** `new-fork` (`PAUSE_S=30`) | — | **2 200** | 2 234 | 10 |
| **Linux** `new-spare`, golden `benchs` (192 MiB) | — | 85 | 100 | 10 |
| **Linux** `new-fork`, golden `benchs` (192 MiB) | — | 2 328 | 2 967 | 10 |
| **macOS** `existing` | 718 | **322** | 379 | 20 (antes: 10) |
| **macOS** `existing`, `KLING_DB_BRANCH_KEEP_PAUSED=1` | — | **57** | 62 | 20 |
| **macOS** `new-spare` | — | **348** | 390 | 20 |
| **macOS** `new-fork` | 1 116 | **731** | 778 | 20 (antes: 10) |

- **Linux**: CT 105 (Proxmox LXC, i7-8700T, 4 CPU, 8 GiB, ext4 sobre LVM + almacén
  Btrfs de copia al escribir), daemon y extensión de esta rama. Golden `pg` (Postgres
  16, 1 GiB configurado, tabla de 400 000 filas). Los "antes" de Linux son los que midió
  el usuario ese mismo día con main; `antes-linux-traza.txt` tiene la traza por fases
  del código de main (con solo la instrumentación añadida) y el desglose del daemon.
- **macOS**: MacBook M4 (16 GiB), backend vz, daemon propio en una raíz aparte
  (`/tmp/kbm`), golden `pg` hecho con `scripts/db-golden.sh build -from` (1 GiB, seed de
  ~70 MiB). El "antes" (`mac-antes/`) es la extensión de main contra el daemon de esta
  rama: no incluye lo que cambió el núcleo (en vz, el fork ya no hashea el overlay).
- Una rama nueva de `new-spare` en Linux (la 3.ª) no pudo usar la reserva (4 619 ms):
  el autovacuum de `template1` al minuto de arrancar cambió la huella del padre. Lo
  arregla `db-golden.sh` en esta misma rama; el golden `pg` medido es anterior.
- `new-fork` en Linux: el volcado de 1 GiB de memoria pasa de ~1,1 s a ~3,5 s cuando
  el disco aún está escribiendo los congelados de la vuelta anterior (ver el log del
  daemon: `commit ...: overlay 29 ms, dump 3248 ms, holes 526 ms`). Con 30 s de
  reposo entre medidas, 2,2 s. Con el golden `benchs` (192 MiB configurados) el volcado
  sigue en ~1,1 s (`dump 1080 ms, holes 86 ms`): lo marca el disco, no el tamaño
  configurado.

## Dónde se va el tiempo ahora (trazas `KLING_DB_TRACE=1` de cada medida)

Linux, volver a una rama (`linux/trace-existing-*.txt`): git 2 ms, listar 1 ms,
`thaw` por el API 13-17 ms en 18 de 20 (42 y 46 ms en las otras dos; despertar 12-14
ms: spawn 3, socket 1-2, load 2-3, resync 4-5), escribir la conexión <1 ms; el resto
hasta los ~43 ms es arrancar `kling` y la extensión desde el gancho (~20 ms: `kling`
solo ya tarda ~11 ms en arrancar, y lanza la extensión dos veces, una para leer su
manifiesto).

Linux, rama nueva con reserva (`linux/trace-new-spare-*.txt`): huella del padre
(`kling exec` + psql) 20-40 ms, cambiar las etiquetas de la reserva <1 ms, despertarla
~18 ms.

macOS: `thaw` de una copia de 1 GiB = ~230 ms de `load` (restaurar el estado en
Virtualization.framework); reanudar una pausada, 0,3 ms en el daemon.

## Datos

- `linux/`, `linux-fork-aislado/`, `linux-benchs/`: CSV (iteración, ms), `summary.txt` y la traza de
  cada medida.
- `mac/`, `mac-keep-paused/`, `mac-antes/`: lo mismo en el M4.
- `antes-linux-traza.txt`: el perfil de partida.

Reproducir:

```sh
GOLDEN=pg N=20 ./scripts/bench-db-branch.sh                     # los tres casos
PAUSE_S=30 CASES=new-fork N=10 ./scripts/bench-db-branch.sh     # fork con el disco en reposo
KLING_DB_BRANCH_KEEP_PAUSED=1 CASES=existing ./scripts/bench-db-branch.sh
```
