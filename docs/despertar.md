# Latencia de despertar: de congelada a primera respuesta

Cuánto tarda una réplica dormida en contestar su primera petición, fase por
fase, y qué se hizo para bajarlo. Método: instrumentar primero, luego cada
palanca con su medida antes/después y su puerta (entra solo si baja lo medido
sin romper nada); lo que no funcionó también está contado.

Todo medido en el mismo sitio que [chispa-serverless.md](chispa-serverless.md):
Intel **i7-8700T**, Proxmox CT 105 (Debian 12, kernel 6.17, KVM sin anidar,
NVMe), Firecracker 1.17 **con jailer**. La carga es una tarea Chispa de 28
etiquetas (`kling chispa deploy -mem 128 -vcpus 1`, dorado de ~70 MiB) detrás de
`kling ai serve`, 10 ciclos por medida con `scripts/99-thaw-bench.sh`.

## Resultado

| | Antes | Congelada (warm) | Pausada |
|---|---|---|---|
| **Réplica dormida → decisión, visto por el cliente (p50)** | **152 ms** | **27 ms** | **2,5 ms** |
| RAM mientras duerme | 0 (en disco) | 0 (en disco) | 36 MiB de RSS (32 de ellos caché del `mem.file`) |

Desglose (media de `kling_ai_wake_phase_seconds`, ms):

| Fase | Antes | Congelada | Pausada | Qué es |
|---|---|---|---|---|
| list | 0,45 | 0,44 | 0,47 | `List` de la flota para elegir máquina |
| renew | 0,30 | 0,27 | 0 (en segundo plano) | renovar el TTL |
| daemon: wait | 0,08 | 0,09 | 0 | candado de la máquina y puerta de arranque |
| daemon: check | 0,41 | 0,47 | 0 | ¿queda un VMM vivo? (escaneo de `/proc`) |
| daemon: net | **47,3** | 0,02 | 0 | namespace, veth, tap, reglas |
| daemon: spawn | 13,6 | 9,7 | 0 | lanzar jailer + preparar el chroot |
| daemon: socket | 10,7 | 4,5 | 0 | esperar al socket de la API del VMM |
| daemon: load | 2,6 | 2,6 | 0,3 | `LoadSnapshot` (lo que mide `thaw_ms`) / `Resume` |
| daemon: resync | **54,2** | 5,7 | 0 | reloj y entropía del invitado |
| daemon: cgroup | 8,2 | 0 | 0 | meter el VMM en su cgroup |
| ready | 0,29 | 0,25 | 0 (no hace falta) | sondeo del puerto del invitado |
| primera petición | 14,0 | 2,5 | 0,5 | `POST /v1/classify` a la réplica |
| **total** | **152,6** | **26,8** | **~2** | |

`thaw_ms` (`LoadSnapshot`) nunca fue el problema: 2-3 ms de 150.

## Las palancas, una a una

Cada fila es una medida con el binario de ese paso; entre paréntesis, la fase
que movió.

| # | Palanca | Antes → después | Entró |
|---|---|---|---|
| 1 | **Instrumentar**: `api.Machine.Wake` (daemon), `scheduler.WakeTrace` (planificador), primera petición en el gateway; `kling thaw` y `kling events` lo enseñan, `/metrics` lo acumula | — | sí |
| 2 | Sondear el socket del VMM cada 1 ms (antes 10) los primeros 200 ms | socket 10,7 → 3,9 | sí |
| 3a | `POSIX_FADV_WILLNEED` del `mem.file` al empezar el thaw | resync 55 → 55: **nada** (el kernel acota ese readahead a su ventana) | no |
| 3b | **Leer** el `mem.file` en segundo plano al empezar el thaw (saltando huecos) | resync 55 → 5,9, primera petición 14 → 2,7; total 146 → 83 | sí |
| 4 | **Conservar la red al congelar** (`internal/machine/red.go`) | net 47 → 0,02; pero la lectura de 3b ya no tiene 47 ms con los que solaparse: resync 5,9 → 21. Total 83 → 53 | sí |
| 5 | El VMM nace en su cgroup (`CLONE_INTO_CGROUP`) en vez de moverlo después | cgroup 6,9 → 0 | sí |
| 6a | No soltar de la caché el `mem.file` de las pequeñas al congelar | nada: tras volcar y perforar solo quedaban 832 KiB de 71 MiB en caché | no |
| 6b | **Leerlo entero al congelar** (≤ 128 MiB), en el segundo plano del segador | resync 18 → 5,7, primera petición 8 → 2,5; total 48 → 31 | sí |
| 7 | Borrar el chroot del jail al congelar, no al descongelar | spawn 14,9 → 9,7; total 31 → 27 | sí |
| 8 | MAC fija del tap0 (`06:00:AC:10:00:01`) para que la caché ARP congelada siga valiendo | primera petición tras rehacer la red: 2,3-2,5 ms con MAC aleatoria, 2,2-2,6 con fija: **sin diferencia** (la ARP del host al invitado ya le corrige la entrada en la misma vuelta). Se queda porque no cuesta nada (va en el mismo `ip link set`) | sin ganancia |
| 9 | Rehacer la red más barato (solo tras reiniciar el daemon o pasados 30 min): el veth nace en su namespace (moverlo costaba 14 ms de RCU), `ip -n` en vez de `ip netns exec ip`, y las reglas de none/internet en un solo `iptables-restore` | net (rehecha) 47 → 23 | sí |
| 10 | **Nivel pausada** (`kling pause`, `-paused-mib`) | 27 → 2,5 ms | sí |
| 11 | Tras reanudar, sin sondeo de puerto y con la renovación del TTL en segundo plano | ready 0,2 → 0, renew 0,2 → 0 | sí |
| — | Filtrar `List` por etiqueta en el daemon | 0,45 ms con 10 máquinas: no merece otra ruta de API | no |
| — | Keep-alive del gateway hacia las réplicas | ≤ 0,2 ms; y una conexión reutilizada hacia una réplica congelada y rehecha falla al escribir, no al conectar, que es lo único que `postGuest` reintenta | no |

Y dos cosas encontradas por el camino:

- **El cliente de la API de Firecracker dejaba una conexión abierta por
  llamada.** Con máquinas que mueren al congelar no se notaba; con pausar y
  reanudar el MISMO VMM, a la quinta vuelta su API rechazaba la siguiente
  (`write: broken pipe`). `internal/fc` ya no usa keep-alive.
- **`kling chispa deploy -mem 64` (el valor por defecto) no basta para un
  modelo de 28 etiquetas con 2^18 cubos**: el calentamiento tardó 1,96 s y las
  réplicas rechazaban conexiones durante segundos tras cada thaw. Con `-mem 128`,
  110 µs. Un modelo de pocas etiquetas sí cabe en 64.

## Cómo está hecho

**Red conservada.** Freeze ya no desmonta el namespace, el veth, el tap0 ni sus
reglas; Thaw comprueba que siguen (`/var/run/netns/<ns>` y el veth del host) y
que los montó este mismo proceso del daemon (`redMontada`), y si no, los rehace
como siempre. El vigilante suelta la red (y la caché de su `mem.file`) de las
que llevan más de 30 min congeladas; un reinicio del daemon desmonta la de todas
(reconciliación de siempre). Coste: un namespace con veth, tap y 3 reglas por
máquina congelada reciente — kilobytes de memoria del kernel.

**Caché del `mem.file`.** Freeze lee entero el volcado de las máquinas pequeñas
(≤ 128 MiB asignados) para dejarlo en la caché de página, y suelta el de las
grandes como antes. Thaw, además, lee en segundo plano el de cualquiera de
hasta 512 MiB por si la caché se perdió. Es memoria reclamable: el kernel la
suelta bajo presión.

**Nivel pausada.** `POST /machines/{ref}/pause` (capacidad `pause`, `kling
pause`) deja el VMM vivo con los vCPU parados, estado `paused`; `thaw` sobre una
pausada es un `Resume` (sin resync: es la misma máquina sin clones, y el reloj
del invitado sigue al del host). Una pausada cuenta como viva para el vigilante,
el TTL (que la congela), la reconciliación (sobrevive a un reinicio del daemon
sin despertarse) y los VMM huérfanos. Freeze, Stop y Remove la aceptan.

En el planificador, el segador duerme cada réplica ociosa así: puntuación =
popularidad / `mem_mib`; de mayor a menor, se pausan mientras la suma de
`mem_mib` pausada quepa en `PausedMiB`; el resto se congela. Una pausada sin uso
durante `PausedFor` (por defecto 10 × idle) se congela de verdad, y con el host
sin memoria `evictLRU` congela las pausadas antes que nada despierto. `kling ai
serve -paused-mib 256` por defecto: caben dos tareas Chispa de 128 MiB y ningún
VON (1,5 GiB); `-paused-mib 0` vuelve a congelarlo todo.

**Sin jailer** (`KLING_JAILER=0`) el thaw congelado baja de ~20 a ~13 ms en el
daemon: spawn pasa de 7,6 a 0,5 ms (jailer copia el binario de Firecracker al
chroot y prepara `/dev` en cada lanzamiento). Se mantiene el jailer por
defecto: el aislamiento vale esos 7 ms.

## Medirlo

```sh
# en el host Linux, con un dorado de tarea Chispa
sudo SNAP=mi-tarea ./scripts/99-thaw-bench.sh daemon    # freeze/thaw por la API del daemon
sudo SNAP=mi-tarea ./scripts/99-thaw-bench.sh gateway   # réplica congelada por el segador
sudo SNAP=mi-tarea ./scripts/99-thaw-bench.sh paused    # réplica pausada (y su RSS)
```

En marcha, `kling thaw <máquina>` imprime el desglose del daemon, `kling events`
lo lleva en el mensaje de cada thaw, el log de `kling ai serve` tiene una línea
por despertar (`replica ready (thaw 23.3 ms: list 0.4, ... ), first request 2.7
ms`) y `/metrics` el histograma `kling_ai_wake_phase_seconds{model,how,phase}`.

## Lo que queda

- **spawn + socket (~13 ms con jailer)** es ya la mitad del thaw congelado. La
  vía es un fondo de VMM ya lanzados (jailer hecho, socket abierto) esperando un
  `LoadSnapshot`; no probado.
- **resync (~5,5 ms)** es el invitado ejecutando por primera vez tras restaurar
  (fallos de página EPT sobre la caché): no es red ni espera.
- La primera petición tras un thaw (2,5 ms) frente a una réplica despierta
  (~0,5 ms) es lo mismo: páginas del invitado tocadas por primera vez.
- Sin medir en Mac (`vz`): allí no hay namespaces ni cgroups; el nivel pausada
  debería funcionar igual, pero no está medido.
