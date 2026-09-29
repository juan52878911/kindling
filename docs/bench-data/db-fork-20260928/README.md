# Base de datos desechable por test — 2026-09-28 (Fase 1 de kling db)

> Repetido con el almacén de copia al escribir en `../db-fork-20260929-cow/`: 1 copia en 133 ms,
> 32 copias en 1,2 s por ronda sin crecer, 3,6 MiB de disco por copia.

Host: CT 105 del laboratorio (Proxmox LXC, Firecracker anidado, 8 GiB, **ext4**, 3,8 GiB
de disco libre). Mismo seed en todos los modos: tabla `items` de 400 000 filas (70 MB en
disco, `seed-bench.sql` de `ext/db/scripts`). La latencia va desde que se suelta la ráfaga
hasta que el primer `count(*)` devuelve 400 000; incluye aplicar el seed en docker y
template. Método y reglas de honestidad: `docs/thaw-at-scale.md`; datos crudos en las
carpetas de al lado.

| modo | 1 copia (p50) | 8 copias (p50 por ronda) | RAM que cuestan 8 copias | disco por copia |
|---|---:|---:|---:|---:|
| kindling `run -from` (Postgres caliente en un golden) | 267 ms (1.ª en frío: 1,8 s) | 533 – 841 ms | ~137 MiB | 211 MiB |
| docker `postgres:16-alpine` + seed (como Testcontainers) | ~3 000 ms | 6 127 – 6 761 ms | ~725 MiB | 204 MiB |
| `CREATE DATABASE … TEMPLATE` | ~229 ms | 1 543 – 1 693 ms | ~19 MiB | n/a |

32 copias de kindling (2.ª pasada, con 11 GiB de disco libre, `kindling-32/`): **DEGRADED**,
69 de 96 copias listas, p50 10,9 s. Los 27 fallos son la **admisión de memoria del daemon**
("doesn't fit: the microVM asks for … MiB"): el golden está configurado a 1 GiB por copia y
el host de 8 GiB no reserva sitio para 32 aunque cada copia use de verdad ~17 MiB de PSS;
las que entran esperan su turno de arranque. Para 32 copias en este host el golden debe
configurarse a ~200 MiB (`mem_mib` es el límite de densidad, no el consumo real). Sin medir
aún con esa configuración.

**Con el golden configurado a 192 MiB** (`kindling-192m*/`): 1 copia en **191 ms**
(p50; la primera en frío 1,7 s), 8 copias 1 032 – 1 252 ms, y **32 copias: 96/96 listas**
en las tres rondas, ~19 MiB de RAM real por copia. Pero la latencia crece de ronda en
ronda (p50 6,0 s → 9,9 s → 15,0 s): cada ronda escribe 32 copias completas del overlay
(~6,7 GiB en ext4), y ese es el cuello de botella a esta escala. Con reflink desaparecería;
sin medir.

Resto de celdas de 32 copias: kindling y docker quedan `skipped` por la regla de
RAM/disco del banco; `TEMPLATE` con 32 llena el disco (desde PG 15 la copia pasa por el
WAL) y el servidor muere, así que su fila `DEGRADED` no es un resultado de Postgres.

## Dónde se van los 267 ms de kindling

- 76 ms: copiar el overlay de 200 MiB del golden. En ext4 es copia completa; con reflink
  (XFS, Btrfs, APFS) sería ~1 ms. **Sin medir con reflink.**
- 9 ms: thaw (`instantiated from benchdb in 9 ms`).
- ~35 ms: red, namespace y CLI.
- ~147 ms: conexión SCRAM y el primer `count(*)` de 400 000 filas (igual en los tres modos).

## Puerta de la Fase 1

| criterio del plan | resultado | |
|---|---|---|
| ≥ 10× más rápido que docker/Testcontainers | 11× (1 copia), 8–12× (8 copias) | ✅ |
| ≥ 3× más rápido que `TEMPLATE` | no con 1 copia; 2–3× con 8 | ⚠️ solo en paralelo |
| p50 < 200 ms | 267 ms en ext4 | ❌ (por verificar con reflink) |
| RAM por copia | ~17 MiB frente a ~90 MiB de docker | ✅ |
| 0 corrupciones, aislamiento | `scripts/db-golden-verify.sh`: 4 copias, reloj bien, SCRAM, `random()` y `gen_random_uuid()` distintos, cada copia ve solo sus escrituras | ✅ |

**Decisión: GO condicionado.** La ventaja frente a docker está demostrada; frente a
`TEMPLATE` solo en paralelo, a cambio de lo que `TEMPLATE` no da (un servidor aparte por
copia: acciones destructivas, superusuario, cualquier motor). No se anuncia la velocidad
de copia hasta medir en un sistema de ficheros con reflink.
