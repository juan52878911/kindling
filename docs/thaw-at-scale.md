# Thaw at scale

What happens when many MCP sessions arrive at once at services that are **frozen**
(scaled to zero), and what it costs the host. [`despertar.md`](despertar.md) answers
"how long does ONE frozen replica take to answer" (27 ms, phase by phase); this page
answers the follow-up question from the r/mcp thread: *and with 50 services and 200
concurrent agents?*

> **Estado: matriz corrida el 2026-11-13 en kling-ascci (CT 108, 10 GiB), con 26 de
> 48 celdas medidas.** Las otras 22 no caben en la RAM de ese host según la regla
> del 70 % y aparecen como `skipped` con la cuenta. Ver [Resultados](#results).

## What is measured

One **cell** is: `M` concurrent MCP sessions, spread round-robin over `N` services, each
session doing the full client dance through the real gateway:

1. `initialize` (the gateway places the session: adopt a running instance, thaw a
   frozen one, restore a new replica from the golden snapshot, or answer 503 at the
   replica cap),
2. `notifications/initialized`,
3. one `tools/call` (the `echo` tool),
4. `CALLS` more `tools/call` (steady state, default 5),
5. `DELETE` of the session.

All `M` sessions wait on a start barrier, so the burst is a burst and not a ramp.

Per cell, [`kling-mcpbench`](../ext/mcp/cmd/kling-mcpbench) reports:

| Figure | Meaning |
|---|---|
| **TTFR** p50/p95/p99/max | `initialize` sent → first tool result received. The headline number: what an agent opening a session on a cold service waits. |
| initialize, first call, steady, delete | The same, per phase. Steady is the hot path, with nothing to wake. |
| ok / errors | Sessions that completed every step, and failures by `phase:code` (`init:http_503`, `call:timeout`...). |
| live peak, machines | Max microVMs of the measured services alive at once, and distinct machines seen (primaries + replicas). |
| PSS peak | Sum of PSS of those microVMs (`kling_machine_pss_mib`), MiB. PSS, not RSS: copies of the same snapshot share pages. |
| min available | Lowest host `MemAvailable` during the cell. |
| PSI max | Highest `/proc/pressure/memory` `some avg10`. The daemon refuses new machines above `KLING_MAX_MEM_PRESSURE` (20 by default). |
| t→0 | After the round, time until no microVM of the measured services is alive (the gateway froze them all). Frozen cells only. |

Percentiles are nearest-rank over the full sorted sample (no histogram buckets, no
interpolation): every percentile is a latency that actually happened. Host samples come
from the daemon's `/metrics` every 250 ms and from `/proc/pressure/memory`.

Every JSON report carries a host header: kernel, CPU model and count, memory, whether the
host is itself a VM (nested virtualization), whether it runs in a container (LXC CT),
kling and Firecracker versions, the git SHA of the bench binary, and the daemon's PSI
limit.

## The matrix

| Axis | Values (default) | Notes |
|---|---|---|
| `N` services | 1, 10, 50 | `tb-0..N-1` (norep) and `tb-r0..N-1` (rep), all from one image `tb-echo`: the example stdio server ([`examples/mcp/stdio-server`](../examples/mcp/stdio-server)), static, behind `kling-bridge`. |
| `M` sessions | 1, 10, 50, 200 | Total, round-robin over the services (with `M < N`, only `M` services are touched). |
| mode | frozen, warm | **frozen**: a priming round creates the instances (and replicas), the gateway freezes them all after `-idle`, then the measured round thaws them. **warm**: priming round and, without waiting, the measured round. |
| replicas | norep, rep | Chosen by memory, because that is what decides sessions per instance (64 MiB per session, 192 reserved, max 32). **norep**: 1024 MiB → 13 sessions per instance. **rep**: 256 MiB → 1 session per instance, so every extra session on a service is a replica. |
| repetitions | R = 3 | Every repetition is its own row. |

The gateway runs with `-max-replicas 16` (the new default, see below), so a rep cell with
more than 16 sessions on one service **expects** 503s. Those rows are kept, not hidden:
that is exactly what the cap is for.

## Honesty rules

- **Every measured run goes in the table**, including failed and degraded ones. No
  reruns until it looks good; no outlier removal.
- A run with **more than 1 % failed sessions is DEGRADED**. Its latency figures are over
  the sessions that completed, so they must not be quoted as the cell's latency.
- A cell whose estimate (instances × configured memory) exceeds **70 % of available
  RAM**, or whose frozen memory files would not fit on disk, **is not run**. It appears
  as `skipped` with the arithmetic, so a missing number is visibly missing and not
  silently absent.
- Between cells the driver waits for zero live `tb-` microVMs and PSI < 5, so one cell's
  backlog does not bleed into the next. If the host does not calm down in 5 minutes the
  run goes on and says so in its log.
- The raw JSON of every run and the gateway log are kept next to the table
  (`docs/bench-data/thaw-scale-<date>/`).
- The host header travels with the numbers. Nested virtualization and an LXC container
  both change the result; a number without them is not comparable.

## The replica cap (safety fix that came with this)

Before this work the MCP gateway had **no replica cap**: `MaxReplicas` was 0 (unlimited),
so 200 simultaneous sessions against one 256 MiB service tried to create ~200
microVMs, and the only brake was the host running out of memory. `kling mcp serve` now
takes `-max-replicas N` (default **16** per service, 0 = unlimited as before). Sessions
beyond the cap get `503 ... all replicas full`, which a client can retry, and the
service is **not** marked unhealthy for it (it is full, not broken). 16 replicas of a
2 GiB service are still 512 concurrent sessions.

## Reproduce

The lab host has no Go. Cross-compile on a workstation, copy, run:

```sh
# On the workstation, from the repo root (amd64 lab; use GOARCH=arm64 for an arm64 host)
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
go build -o /tmp/tb/stdio-server ./examples/mcp/stdio-server
(cd ext/mcp && go build -o /tmp/tb/kling-mcpbench ./cmd/kling-mcpbench \
             && go build -o /tmp/tb/kling-bridge  ./cmd/kling-bridge \
             && go build -o /tmp/tb/kling-mcp     ./cmd/kling-mcp)
cp ext/mcp/scripts/95-thaw-scale.sh ext/mcp/scripts/80-mcp-image.sh /tmp/tb/
scp -r /tmp/tb juan@<lab>:~/tb

# On the lab host (user with passwordless sudo, next to the running daemon)
cd ~/tb && KLING_MCP=./kling-mcp ./95-thaw-scale.sh
```

`kling-bridge` must be the one the image was built for; if the lab already has one
installed (`make bridge`), `BRIDGE=/path/to/it` works too. The run needs the base image
(`min` by default, `BASE_IMAGE=` to change it) and free disk for the golden snapshots
(`N × memory` per replica variant, plus 3 GiB); the preflight checks both and refuses
otherwise.

A short smoke pass first is a good idea:

```sh
NS="1 10" MS="1 10" R=1 KLING_MCP=./kling-mcp ./95-thaw-scale.sh
```

Useful knobs (all environment variables, see the script header): `NS`, `MS`, `MODES`,
`REPS`, `R`, `CALLS`, `IDLE` (gateway `-idle`, 10 s), `SETTLE` (max wait for t→0,
120 s), `MAXREP` (16), `MEM_NOREP` (1024M), `MEM_REP` (256M), `KEEP=1` (leave
everything in place). The driver starts its **own** gateway on `127.0.0.1:18180` with a
one-off token passed by environment, so it does not touch the production gateway or its
token; it removes every `tb-` machine, template and the `tb-echo` image on exit and
checks that the host is back to its baseline.

To run a single cell by hand against an existing gateway:

```sh
KLING_GATEWAY_TOKEN=... ./kling-mcpbench -gateway http://127.0.0.1:8080 \
    -services tb-0..9 -sessions 50 -calls 5 -settle 60s -label manual -json out.json
```

## Results

Matriz completa por defecto (`NS="1 10 50" MS="1 10 50 200"`, frozen y warm, norep y
rep, R = 3, `CALLS=5`, `IDLE=10s`, `MAXREP=16`), corrida una sola vez de principio a
fin el 2026-11-13 de 02:27 a 03:22 UTC. Resultado del driver: **78 rondas medidas, 18
DEGRADED, 22 celdas skipped, 0 errores del banco**, y limpieza con la línea base
correcta (`baseline ok: no tb- leftovers, MemAvailable 9955 MiB (was 9952)`). No se
repitió ni se quitó ninguna ronda.

**Host** (cabecera de cada JSON): `kling-ascci`, CT 108 LXC privilegiado en el nodo
Proxmox ascci, Firecracker **sin** virtualización anidada (`nested: false`,
`container: lxc`), kernel `7.0.14-12-pve`, Intel Core i3-3220 a 3,30 GHz (4 hilos, sin
AVX2), 10 GiB de RAM (~9,9 GiB disponibles al empezar), Firecracker v1.17.0,
backend firecracker, amd64. Raíz de datos en un dataset ZFS (`tank/kindling`, clones
por reflink). kindling **1.0.0-rc.2-5-gc4bc2038** (`main` en c4bc2038): daemon,
kling-mcp, kling-mcpbench y kling-bridge compilados de ese commit. Límite de PSI del
daemon: 20 (por defecto). Ni swap en uso ni presión de memoria: PSI some avg10 = 0,0 en
todas las rondas.

Cómo se corrió: un daemon **privado** (`kling daemon -root
/var/lib/kindling/kt-thaw/root -socket /run/kt-thaw/kling.sock`) junto al daemon de
systemd del host, que siguió sirviendo sin tocarse; el script, con
`KLING=<kling de la rama> KLING_HOST=unix:///run/kt-thaw/kling.sock
SOCK=/run/kt-thaw/kling.sock KLING_ROOT=/var/lib/kindling/kt-thaw/root
KLING_MCP=./kling-mcp`, todo bajo `flock /tmp/lab.lock`, el cerrojo que toman todos los trabajos del lab; los
otros daemons del host estaban ociosos. Antes, a las 02:09, una pasada de humo en el
mismo host (`NS=1 MS="1 10" R=1`) comprobó la tubería; no forma parte de la tabla.

Datos crudos: [`docs/bench-data/thaw-scale-20261113/`](bench-data/thaw-scale-20261113/)
(un JSON y un log por ronda, el log de cebado, `gateway.log`, `results.md` y
`driver.log`, la salida completa del script).

### Lo que dicen los números

- **Despertar una instancia sola cuesta ~0,5–0,8 s en este host.** Las 18 rondas
  frozen con M = 1 (N 1/10/50, norep y rep) dan un TTFR de 333–808 ms; las mismas en
  warm, 11–15 ms (rep) y 17–104 ms (norep). Es unas 20 veces la cifra de 26 ms de la
  pasada de humo en el CT 105 y de [`despertar.md`](despertar.md). La causa no se midió
  aquí. Un dato que encaja: el daemon avisó en cada congelación de que
  `/var/lib/kindling/kt-thaw/root/machines does not tell holes from zero pages;
  freezing the whole memory`, así que sobre este ZFS cada instancia congelada guarda
  su memoria entera (1 GiB en norep, 256 MiB en rep). No se comprobó si eso explica la
  diferencia.
- **Diez réplicas despertando a la vez** (rep, M = 10, N 1/10/50): TTFR p50 de
  760–1329 ms y p95 de 1104–2478 ms, sin un solo fallo. El PSS pico de las 10 microVMs
  fue de 131–147 MiB (~14 MiB cada una), y la memoria disponible no bajó de 9854 MiB.
- **Estado estable** (el camino caliente, sin nada que despertar): p50 de 1,4–29 ms en
  todas las rondas medidas.
- **El tope de réplicas funciona** como está escrito arriba: rep con N = 1 y M = 50 o
  200 deja pasar exactamente 16 sesiones (las 16 réplicas) y responde 503 al resto.
  Esas 12 rondas son DEGRADED por diseño, y su latencia es la de las 16 que entraron.
- **Hallazgo: con una ráfaga, el gateway crea réplicas de más.** norep con N = 1 y
  M = 50: con 13 sesiones por instancia bastarían 4 instancias, pero en las 6 rondas el
  gateway levantó 16 (`live peak 16`, el tope) y respondió 503 a 23 de 50 sesiones en
  frozen (46 %) y a 3–4 en warm (6–8 %). La estimación previa del script (4 × 1024 MiB)
  dejó correr la celda; la memoria configurada real fue de 16 × 1024 MiB = 16 GiB en un
  host de 10 GiB. El PSS pico se quedó en 350–468 MiB porque la memoria se reserva sin
  tocarla, pero la regla del 70 % se basa en la memoria configurada y aquí no se
  cumplió. Esas 6 rondas son DEGRADED y su latencia no es la de la celda. Hace falta
  seguimiento aparte (la colocación de sesiones concurrentes mientras nace una
  réplica).
- **De vuelta a cero** (t→0, `-idle 10s`): 12–15 s con una instancia, 26–42 s con
  10–16.

### Celdas que no se corrieron

22 celdas (frozen y warm cuentan por separado) quedaron `skipped` por la regla del
70 %: la estimación es *instancias × memoria configurada*, y el 70 % de la RAM
disponible eran ~6,7–6,9 GiB. El disco nunca fue el límite (~597 GiB libres).

| variante | celdas skipped | por qué |
|---|---|---|
| norep (1024 MiB, 13 sesiones/instancia) | N=1 M=200 | 16 instancias × 1024 = 16384 MiB |
| norep | N=10 M=10, N=10 M=50 | 10 × 1024 = 10240 MiB |
| norep | N=10 M=200 | 20 × 1024 = 20480 MiB |
| norep | N=50 M=10 | 10 × 1024 = 10240 MiB |
| norep | N=50 M=50, N=50 M=200 | 50 × 1024 = 51200 MiB |
| rep (256 MiB, 1 sesión/instancia) | N=10 M=50 | 50 × 256 = 12800 MiB |
| rep | N=10 M=200 | 160 × 256 = 40960 MiB |
| rep | N=50 M=50 | 50 × 256 = 12800 MiB |
| rep | N=50 M=200 | 200 × 256 = 51200 MiB |

Cada fila vale para frozen y para warm. La pregunta de partida, *50 servicios y 200
agentes*, cae justo en estas celdas y **sigue sin respuesta medida**. Los ~14–30 MiB de
PSS por microVM medidos sugieren que en la práctica cabrían, pero eso es una
extrapolación y no se publica como resultado. Para cerrarlas hace falta un host con
más RAM (unos 75 GiB disponibles para la celda más grande de norep) o una corrida
aparte con `MEM_NOREP`/`MEM_REP` más bajos, que sería otra matriz y no esta.

### Tabla completa

Cada repetición es su fila; las skipped llevan la cuenta en la columna de estado.
Latencias en ms.

| cell | N | M | ok | TTFR p50 | p95 | p99 | max | steady p50 | p99 | live peak | machines | PSS peak MiB | min avail MiB | PSI max | t→0 s | status |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| norep-frozen-n1-m1-r1 | 1 | 1 | 1/1 | 526 | 526 | 526 | 526 | 1.7 | 3.1 | 1 | 1 | 17 | 9925 | 0.0 | 14.0 | OK |
| norep-frozen-n1-m1-r2 | 1 | 1 | 1/1 | 565 | 565 | 565 | 565 | 1.7 | 2.1 | 1 | 1 | 17 | 9922 | 0.0 | 13.7 | OK |
| norep-frozen-n1-m1-r3 | 1 | 1 | 1/1 | 594 | 594 | 594 | 594 | 2.2 | 3.3 | 1 | 1 | 18 | 9923 | 0.0 | 13.8 | OK |
| norep-warm-n1-m1-r1 | 1 | 1 | 1/1 | 76 | 76 | 76 | 76 | 1.6 | 1.9 | 1 | 1 | 21 | 9932 | 0.0 | n/a | OK |
| norep-warm-n1-m1-r2 | 1 | 1 | 1/1 | 70 | 70 | 70 | 70 | 1.7 | 2.2 | 1 | 1 | 20 | 9936 | 0.0 | n/a | OK |
| norep-warm-n1-m1-r3 | 1 | 1 | 1/1 | 17 | 17 | 17 | 17 | 1.6 | 1.7 | 1 | 1 | 26 | 9928 | 0.0 | n/a | OK |
| norep-frozen-n1-m10-r1 | 1 | 10 | 10/10 | 985 | 1000 | 1000 | 1000 | 17.5 | 92.4 | 1 | 1 | 35 | 9915 | 0.0 | 14.0 | OK |
| norep-frozen-n1-m10-r2 | 1 | 10 | 10/10 | 606 | 800 | 800 | 800 | 7.1 | 98.4 | 1 | 1 | 31 | 9915 | 0.0 | 15.1 | OK |
| norep-frozen-n1-m10-r3 | 1 | 10 | 10/10 | 838 | 932 | 932 | 932 | 10.1 | 81.8 | 1 | 1 | 30 | 9914 | 0.0 | 13.6 | OK |
| norep-warm-n1-m10-r1 | 1 | 10 | 10/10 | 276 | 387 | 387 | 387 | 5.3 | 76.6 | 1 | 1 | 46 | 9906 | 0.0 | n/a | OK |
| norep-warm-n1-m10-r2 | 1 | 10 | 10/10 | 357 | 460 | 460 | 460 | 4.3 | 71.9 | 1 | 1 | 45 | 9910 | 0.0 | n/a | OK |
| norep-warm-n1-m10-r3 | 1 | 10 | 10/10 | 287 | 411 | 411 | 411 | 3.9 | 93.3 | 1 | 1 | 45 | 9903 | 0.0 | n/a | OK |
| norep-frozen-n1-m50-r1 | 1 | 50 | 27/50 | 1473 | 2643 | 2704 | 2704 | 8.1 | 86.7 | 16 | 16 | 350 | 9714 | 0.0 | 36.7 | DEGRADED |
| norep-frozen-n1-m50-r2 | 1 | 50 | 27/50 | 1491 | 2967 | 3069 | 3069 | 14.3 | 118.7 | 16 | 16 | 415 | 9642 | 0.0 | 37.1 | DEGRADED |
| norep-frozen-n1-m50-r3 | 1 | 50 | 27/50 | 1466 | 2800 | 2956 | 2956 | 8.7 | 291.3 | 16 | 16 | 407 | 9646 | 0.0 | 37.3 | DEGRADED |
| norep-warm-n1-m50-r1 | 1 | 50 | 47/50 | 488 | 588 | 601 | 601 | 29.3 | 125.8 | 16 | 16 | 468 | 9596 | 0.0 | n/a | DEGRADED |
| norep-warm-n1-m50-r2 | 1 | 50 | 47/50 | 449 | 587 | 598 | 598 | 21.4 | 156.8 | 16 | 16 | 459 | 9602 | 0.0 | n/a | DEGRADED |
| norep-warm-n1-m50-r3 | 1 | 50 | 46/50 | 418 | 550 | 553 | 553 | 16.6 | 103.7 | 16 | 16 | 467 | 9593 | 0.0 | n/a | DEGRADED |
| norep-frozen-n1-m200 | 1 | 200 | skipped | | | | | | | | 16 (est) | | 9600 | | | skipped: estimated 16 machine(s) × 1024 MiB = 16384 MiB; available 9600 MiB (70% = 6720), disk free 596727 MiB |
| norep-warm-n1-m200 | 1 | 200 | skipped | | | | | | | | 16 (est) | | 9600 | | | skipped: estimated 16 machine(s) × 1024 MiB = 16384 MiB; available 9600 MiB (70% = 6720), disk free 596727 MiB |
| norep-frozen-n10-m1-r1 | 10 | 1 | 1/1 | 627 | 627 | 627 | 627 | 2.4 | 2.8 | 1 | 1 | 18 | 9912 | 0.0 | 13.6 | OK |
| norep-frozen-n10-m1-r2 | 10 | 1 | 1/1 | 526 | 526 | 526 | 526 | 2.1 | 3.7 | 1 | 1 | 18 | 9907 | 0.0 | 13.7 | OK |
| norep-frozen-n10-m1-r3 | 10 | 1 | 1/1 | 619 | 619 | 619 | 619 | 1.8 | 3.2 | 1 | 1 | 18 | 9908 | 0.0 | 13.9 | OK |
| norep-warm-n10-m1-r1 | 10 | 1 | 1/1 | 104 | 104 | 104 | 104 | 3.7 | 68.9 | 1 | 1 | 26 | 9915 | 0.0 | n/a | OK |
| norep-warm-n10-m1-r2 | 10 | 1 | 1/1 | 80 | 80 | 80 | 80 | 2.4 | 2.9 | 1 | 1 | 20 | 9924 | 0.0 | n/a | OK |
| norep-warm-n10-m1-r3 | 10 | 1 | 1/1 | 77 | 77 | 77 | 77 | 1.8 | 2.6 | 1 | 1 | 21 | 9920 | 0.0 | n/a | OK |
| norep-frozen-n10-m10 | 10 | 10 | skipped | | | | | | | | 10 (est) | | 9921 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9921 MiB (70% = 6944), disk free 596731 MiB |
| norep-warm-n10-m10 | 10 | 10 | skipped | | | | | | | | 10 (est) | | 9921 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9921 MiB (70% = 6944), disk free 596731 MiB |
| norep-frozen-n10-m50 | 10 | 50 | skipped | | | | | | | | 10 (est) | | 9922 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-warm-n10-m50 | 10 | 50 | skipped | | | | | | | | 10 (est) | | 9922 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-frozen-n10-m200 | 10 | 200 | skipped | | | | | | | | 20 (est) | | 9922 | | | skipped: estimated 20 machine(s) × 1024 MiB = 20480 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-warm-n10-m200 | 10 | 200 | skipped | | | | | | | | 20 (est) | | 9922 | | | skipped: estimated 20 machine(s) × 1024 MiB = 20480 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-frozen-n50-m1-r1 | 50 | 1 | 1/1 | 690 | 690 | 690 | 690 | 1.6 | 1.8 | 1 | 1 | 18 | 9909 | 0.0 | 12.0 | OK |
| norep-frozen-n50-m1-r2 | 50 | 1 | 1/1 | 333 | 333 | 333 | 333 | 1.8 | 2.8 | 1 | 1 | 18 | 9912 | 0.0 | 12.4 | OK |
| norep-frozen-n50-m1-r3 | 50 | 1 | 1/1 | 682 | 682 | 682 | 682 | 1.7 | 2.4 | 1 | 1 | 18 | 9912 | 0.0 | 12.6 | OK |
| norep-warm-n50-m1-r1 | 50 | 1 | 1/1 | 70 | 70 | 70 | 70 | 1.6 | 2.6 | 1 | 1 | 21 | 9923 | 0.0 | n/a | OK |
| norep-warm-n50-m1-r2 | 50 | 1 | 1/1 | 26 | 26 | 26 | 26 | 1.6 | 52.7 | 1 | 1 | 27 | 9920 | 0.0 | n/a | OK |
| norep-warm-n50-m1-r3 | 50 | 1 | 1/1 | 83 | 83 | 83 | 83 | 1.7 | 2.6 | 1 | 1 | 21 | 9924 | 0.0 | n/a | OK |
| norep-frozen-n50-m10 | 50 | 10 | skipped | | | | | | | | 10 (est) | | 9926 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-warm-n50-m10 | 50 | 10 | skipped | | | | | | | | 10 (est) | | 9926 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-frozen-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9926 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-warm-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9926 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-frozen-n50-m200 | 50 | 200 | skipped | | | | | | | | 50 (est) | | 9925 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9925 MiB (70% = 6947), disk free 596731 MiB |
| norep-warm-n50-m200 | 50 | 200 | skipped | | | | | | | | 50 (est) | | 9925 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9925 MiB (70% = 6947), disk free 596731 MiB |
| rep-frozen-n1-m1-r1 | 1 | 1 | 1/1 | 531 | 531 | 531 | 531 | 1.5 | 1.7 | 1 | 1 | 13 | 9927 | 0.0 | 12.0 | OK |
| rep-frozen-n1-m1-r2 | 1 | 1 | 1/1 | 579 | 579 | 579 | 579 | 1.6 | 1.7 | 1 | 1 | 14 | 9927 | 0.0 | 12.2 | OK |
| rep-frozen-n1-m1-r3 | 1 | 1 | 1/1 | 562 | 562 | 562 | 562 | 1.4 | 1.6 | 1 | 1 | 14 | 9925 | 0.0 | 13.7 | OK |
| rep-warm-n1-m1-r1 | 1 | 1 | 1/1 | 11 | 11 | 11 | 11 | 1.4 | 1.6 | 1 | 1 | 15 | 9930 | 0.0 | n/a | OK |
| rep-warm-n1-m1-r2 | 1 | 1 | 1/1 | 11 | 11 | 11 | 11 | 1.5 | 1.5 | 1 | 1 | 15 | 9931 | 0.0 | n/a | OK |
| rep-warm-n1-m1-r3 | 1 | 1 | 1/1 | 15 | 15 | 15 | 15 | 1.4 | 1.7 | 1 | 1 | 15 | 9930 | 0.0 | n/a | OK |
| rep-frozen-n1-m10-r1 | 1 | 10 | 10/10 | 830 | 1241 | 1241 | 1241 | 2.3 | 47.4 | 10 | 10 | 131 | 9874 | 0.0 | 27.2 | OK |
| rep-frozen-n1-m10-r2 | 1 | 10 | 10/10 | 1010 | 1428 | 1428 | 1428 | 2.5 | 42.5 | 10 | 10 | 145 | 9860 | 0.0 | 31.1 | OK |
| rep-frozen-n1-m10-r3 | 1 | 10 | 10/10 | 994 | 1448 | 1448 | 1448 | 2.4 | 42.5 | 10 | 10 | 142 | 9856 | 0.0 | 29.0 | OK |
| rep-warm-n1-m10-r1 | 1 | 10 | 10/10 | 64 | 81 | 81 | 81 | 7.4 | 21.6 | 10 | 10 | 149 | 9858 | 0.0 | n/a | OK |
| rep-warm-n1-m10-r2 | 1 | 10 | 10/10 | 62 | 83 | 83 | 83 | 8.4 | 28.3 | 10 | 10 | 149 | 9864 | 0.0 | n/a | OK |
| rep-warm-n1-m10-r3 | 1 | 10 | 10/10 | 65 | 77 | 77 | 77 | 9.7 | 18.6 | 10 | 10 | 149 | 9857 | 0.0 | n/a | OK |
| rep-frozen-n1-m50-r1 | 1 | 50 | 16/50 | 1573 | 2178 | 2178 | 2178 | 2.2 | 51.1 | 16 | 16 | 227 | 9812 | 0.0 | 38.8 | DEGRADED |
| rep-frozen-n1-m50-r2 | 1 | 50 | 16/50 | 1433 | 2021 | 2021 | 2021 | 2.4 | 50.1 | 16 | 16 | 230 | 9830 | 0.0 | 40.0 | DEGRADED |
| rep-frozen-n1-m50-r3 | 1 | 50 | 16/50 | 1440 | 2137 | 2137 | 2137 | 2.2 | 38.7 | 16 | 16 | 230 | 9830 | 0.0 | 37.1 | DEGRADED |
| rep-warm-n1-m50-r1 | 1 | 50 | 16/50 | 143 | 177 | 177 | 177 | 14.3 | 51.0 | 16 | 16 | 241 | 9826 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m50-r2 | 1 | 50 | 16/50 | 116 | 133 | 133 | 133 | 14.2 | 28.0 | 16 | 16 | 242 | 9828 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m50-r3 | 1 | 50 | 16/50 | 123 | 142 | 142 | 142 | 14.2 | 37.8 | 16 | 16 | 243 | 9805 | 0.0 | n/a | DEGRADED |
| rep-frozen-n1-m200-r1 | 1 | 200 | 16/200 | 1679 | 2280 | 2280 | 2280 | 2.2 | 43.3 | 16 | 16 | 236 | 9811 | 0.0 | 36.2 | DEGRADED |
| rep-frozen-n1-m200-r2 | 1 | 200 | 16/200 | 1752 | 2340 | 2340 | 2340 | 2.1 | 18.6 | 16 | 16 | 237 | 9789 | 0.0 | 41.6 | DEGRADED |
| rep-frozen-n1-m200-r3 | 1 | 200 | 16/200 | 1570 | 2163 | 2163 | 2163 | 2.2 | 24.2 | 16 | 16 | 236 | 9811 | 0.0 | 37.8 | DEGRADED |
| rep-warm-n1-m200-r1 | 1 | 200 | 16/200 | 196 | 278 | 278 | 278 | 12.9 | 120.9 | 16 | 16 | 248 | 9779 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m200-r2 | 1 | 200 | 16/200 | 234 | 298 | 298 | 298 | 12.9 | 46.4 | 16 | 16 | 247 | 9798 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m200-r3 | 1 | 200 | 16/200 | 235 | 319 | 319 | 319 | 16.9 | 63.9 | 16 | 16 | 252 | 9794 | 0.0 | n/a | DEGRADED |
| rep-frozen-n10-m1-r1 | 10 | 1 | 1/1 | 568 | 568 | 568 | 568 | 2.0 | 2.2 | 1 | 1 | 14 | 9909 | 0.0 | 14.2 | OK |
| rep-frozen-n10-m1-r2 | 10 | 1 | 1/1 | 653 | 653 | 653 | 653 | 1.5 | 1.7 | 1 | 1 | 14 | 9918 | 0.0 | 13.6 | OK |
| rep-frozen-n10-m1-r3 | 10 | 1 | 1/1 | 537 | 537 | 537 | 537 | 1.5 | 1.8 | 1 | 1 | 14 | 9917 | 0.0 | 13.7 | OK |
| rep-warm-n10-m1-r1 | 10 | 1 | 1/1 | 14 | 14 | 14 | 14 | 1.5 | 1.6 | 1 | 1 | 15 | 9922 | 0.0 | n/a | OK |
| rep-warm-n10-m1-r2 | 10 | 1 | 1/1 | 12 | 12 | 12 | 12 | 1.4 | 1.7 | 1 | 1 | 15 | 9922 | 0.0 | n/a | OK |
| rep-warm-n10-m1-r3 | 10 | 1 | 1/1 | 13 | 13 | 13 | 13 | 1.5 | 2.0 | 1 | 1 | 15 | 9922 | 0.0 | n/a | OK |
| rep-frozen-n10-m10-r1 | 10 | 10 | 10/10 | 760 | 1104 | 1104 | 1104 | 1.9 | 34.0 | 10 | 10 | 133 | 9868 | 0.0 | 25.9 | OK |
| rep-frozen-n10-m10-r2 | 10 | 10 | 10/10 | 779 | 1124 | 1124 | 1124 | 2.1 | 44.2 | 10 | 10 | 144 | 9855 | 0.0 | 30.3 | OK |
| rep-frozen-n10-m10-r3 | 10 | 10 | 10/10 | 990 | 1330 | 1330 | 1330 | 2.0 | 53.0 | 10 | 10 | 142 | 9854 | 0.0 | 25.9 | OK |
| rep-warm-n10-m10-r1 | 10 | 10 | 10/10 | 60 | 71 | 71 | 71 | 9.7 | 23.0 | 10 | 10 | 148 | 9857 | 0.0 | n/a | OK |
| rep-warm-n10-m10-r2 | 10 | 10 | 10/10 | 55 | 81 | 81 | 81 | 8.6 | 37.8 | 10 | 10 | 150 | 9872 | 0.0 | n/a | OK |
| rep-warm-n10-m10-r3 | 10 | 10 | 10/10 | 62 | 75 | 75 | 75 | 10.7 | 26.8 | 10 | 10 | 150 | 9863 | 0.0 | n/a | OK |
| rep-frozen-n10-m50 | 10 | 50 | skipped | | | | | | | | 50 (est) | | 9874 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-warm-n10-m50 | 10 | 50 | skipped | | | | | | | | 50 (est) | | 9874 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-frozen-n10-m200 | 10 | 200 | skipped | | | | | | | | 160 (est) | | 9874 | | | skipped: estimated 160 machine(s) × 256 MiB = 40960 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-warm-n10-m200 | 10 | 200 | skipped | | | | | | | | 160 (est) | | 9874 | | | skipped: estimated 160 machine(s) × 256 MiB = 40960 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-frozen-n50-m1-r1 | 50 | 1 | 1/1 | 743 | 743 | 743 | 743 | 1.7 | 2.7 | 1 | 1 | 15 | 9922 | 0.0 | 14.5 | OK |
| rep-frozen-n50-m1-r2 | 50 | 1 | 1/1 | 701 | 701 | 701 | 701 | 1.5 | 1.7 | 1 | 1 | 14 | 9922 | 0.0 | 13.5 | OK |
| rep-frozen-n50-m1-r3 | 50 | 1 | 1/1 | 808 | 808 | 808 | 808 | 1.5 | 1.8 | 1 | 1 | 14 | 9923 | 0.0 | 12.2 | OK |
| rep-warm-n50-m1-r1 | 50 | 1 | 1/1 | 12 | 12 | 12 | 12 | 1.4 | 1.6 | 1 | 1 | 15 | 9928 | 0.0 | n/a | OK |
| rep-warm-n50-m1-r2 | 50 | 1 | 1/1 | 13 | 13 | 13 | 13 | 1.6 | 1.8 | 1 | 1 | 15 | 9927 | 0.0 | n/a | OK |
| rep-warm-n50-m1-r3 | 50 | 1 | 1/1 | 12 | 12 | 12 | 12 | 1.6 | 1.7 | 1 | 1 | 15 | 9928 | 0.0 | n/a | OK |
| rep-frozen-n50-m10-r1 | 50 | 10 | 10/10 | 1020 | 1461 | 1461 | 1461 | 2.3 | 6.3 | 10 | 10 | 143 | 9867 | 0.0 | 30.5 | OK |
| rep-frozen-n50-m10-r2 | 50 | 10 | 10/10 | 1329 | 2478 | 2478 | 2478 | 1.7 | 2.2 | 10 | 10 | 147 | 9867 | 0.0 | 29.5 | OK |
| rep-frozen-n50-m10-r3 | 50 | 10 | 10/10 | 1230 | 2315 | 2315 | 2315 | 1.7 | 4.6 | 10 | 10 | 144 | 9866 | 0.0 | 28.7 | OK |
| rep-warm-n50-m10-r1 | 50 | 10 | 10/10 | 55 | 96 | 96 | 96 | 8.4 | 28.4 | 10 | 10 | 150 | 9862 | 0.0 | n/a | OK |
| rep-warm-n50-m10-r2 | 50 | 10 | 10/10 | 74 | 98 | 98 | 98 | 10.2 | 25.8 | 10 | 10 | 150 | 9862 | 0.0 | n/a | OK |
| rep-warm-n50-m10-r3 | 50 | 10 | 10/10 | 58 | 101 | 101 | 101 | 7.5 | 44.2 | 10 | 10 | 151 | 9855 | 0.0 | n/a | OK |
| rep-frozen-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9858 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
| rep-warm-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9858 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
| rep-frozen-n50-m200 | 50 | 200 | skipped | | | | | | | | 200 (est) | | 9858 | | | skipped: estimated 200 machine(s) × 256 MiB = 51200 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
| rep-warm-n50-m200 | 50 | 200 | skipped | | | | | | | | 200 (est) | | 9858 | | | skipped: estimated 200 machine(s) × 256 MiB = 51200 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |

### Pasada de humo anterior (CT 105, no es la matriz)

To check the pipeline end to end, a smoke pass ran on the development lab: a Proxmox LXC
container (CT 105, nested Firecracker, 8 GiB, ext4, 4 GiB of free disk). It is **not** the
result of the matrix and must not be quoted as such:

- `NS=1 MS="1 10" R=1 PSI_MAX=20`. `PSI_MAX` was raised because the container reads the
  pressure of the whole Proxmox host, which sat at 11–15 at rest.
- N=1, M=1, `rep` variant, frozen: **26 ms** from the first request to the first `tools/call`
  result, with 1 microVM woken (31 MiB PSS) and back to zero in 11.7 s (`-idle 10s`).
  The same cell warm took 6 ms, and steady calls took 0.7–0.9 ms.
- All M=10 cells were **skipped** by the preflight estimate: 10 replicas × 256 MiB of
  frozen memory files do not fit in 4 GiB of free disk. They show up as `skipped` in the
  table, as the honesty rules require.

The full matrix needs a host with at least ~40 GiB of free disk for `N=50`.

En kling-ascci sobraba disco y el límite fue la RAM (ver arriba).
