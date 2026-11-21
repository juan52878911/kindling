# Banco reproducible (F0)

Mide kindling (y, para comparar, OpenSandbox en k3s/Docker) con serie, ráfagas c10/c20/c50 y
**tabla por fase** sacada de `/metrics` del daemon y del gateway. Este directorio **no** entra en
el `embed` de `scripts/`.

| Fichero | Para qué |
|---|---|
| `bench.py k\|kpool\|osb\|osbpool` | Una línea JSON: `create_serial_ms`, `create_c10_ms`…, `exec_echo_ms`, pausa/reanudación y `fases` (serie, c10, c20, c50). |
| `fases.py` | Lee `/metrics` antes/después de cada tanda y saca p50/p95/media por fase junto a la latencia del cliente. También `fases.py snap` / `diff a.json b.json`. |
| `bench_k8s.py` | Pod desnudo en k3s (línea base de Kubernetes). |
| `ec2.sh` | Lanza una EC2 anidada, instala, mide, trae resultados y la **borra sola**. `--dry-run` enseña el plan. |
| `remoto/` | Lo que corre dentro de la EC2: `setup.sh`, `fases.sh`, `daemon-up/down.sh`, `gw-up.sh`. |
| `osbk8s/` | Configuración de OpenSandbox sobre k3s. |

## Con un comando (en un host con KVM y un daemon ya levantado)

```sh
KTOK=<token del inquilino> python3 -I scripts/bench/bench.py kpool
```

Variables: `SERIE` (15), `CONC` (`10,20`), `EXECS` (30), `ESPERA_POOL` (s antes de cada ráfaga),
`KB` (`/opt/kbench`), `KB_URL`, `KB_SDK`, `KB_DAEMON_SOCK`, `KB_DAEMON_CONFIG`, `KB_KLING`.
El token nunca va en el repositorio: `remoto/gw-up.sh` lo genera (0600) y no lo imprime.

## En EC2 (cuesta dinero: lo lanza quien decide el gasto)

```sh
scripts/bench/ec2.sh --dry-run kindling          # qué crearía y qué borraría
BINS_DIR=… SDK_DIR=… scripts/bench/ec2.sh kindling
scripts/bench/ec2.sh --limpiar <ID>              # si algo quedó a medias
```

Doble tope: `trap` (EXIT/INT/TERM) termina instancia, grupo de seguridad y clave; y la instancia nace
con `InstanceInitiatedShutdownBehavior=terminate` + `shutdown -h +TOPE_MIN` (300 por defecto).

## Higiene entre variantes y qué trae el JSON

Cada vuelta del barrido (`remoto/fases.sh`) hace `higiene.sh inicio` (apunta el espacio libre),
y al acabar `higiene.sh rm` (`kling rm -f` de todo), parar daemon y gateway, `daemon-down.sh`
(mata los firecracker restantes, borra los netns `kl-*`, desmonta `cow.xfs` y sus loop) y
`higiene.sh verificar`: 0 firecracker, 0 netns `kl-*`, 0 montajes de `cow.xfs` y el mismo espacio
libre (±100 MiB, `TOL_LIBRE_MIB`); si no, el banco aborta. **Solo en la instancia del banco**:
mata procesos por nombre, nunca en el laboratorio.

`bench.py` añade al JSON: `create_c50_ms` y `create_c50b_ms` (con `CONC=…,50,50` por defecto en
`fases.sh`; un tamaño repetido lleva sufijo `b`, `c`…), `exec_echo_rafaga_ms` / `exec_echo_reposo_ms`
y lo mismo para `resume_hasta_exec_*` y `thaw_desde_freeze_hasta_exec_*` (ráfaga = justo al volver,
con el relleno del pool en marcha; reposo = tras `ESPERA_POOL`; las claves sin sufijo son las de
ráfaga, como antes), `resync_serie` (`GET /resync` del agente tras cada creación en serie, con
`critico_us` y pasos; ruta en `KB_RESYNC_RUTA`, `{id}` = máquina; si el daemon no la sirve queda
`{"error": …}`), `vmm_muertos` (líneas `watch:` del journal de la unidad `KB_UNIT` por tanda) y
`diag` (dmesg filtrado y `memory.events` al acabar cada tanda), `net_pool` (`KLING_NET_POOL` de la
corrida), `kvm_serie` (delta de `pf_fixed`/`exits` de KVM por creación en serie; hace falta root y debugfs, si no
`null`) y `pausa_exec_nueva_ms` / `pausa_exec_ociosa_ms` (E5: pausa→exec con un sandbox recién creado y con otro
ocioso `E5_OCIOSA_S`=60 s; `E5_N`=5 muestras). `fases.py --self-test` comprueba la tabla de fases de una tanda de 50.
`bench.py --self-test` lo comprueba con un gateway falso, sin KVM ni root.

## Barrido

`PARALELO="2 4 8 16"` (`KLING_MAX_PARALLEL_BOOT`), `JAILER="0 1"`, `CUOTAS_LISTA="- 200:50"`
(`-` = inquilino sin cuotas; `a:b` = `max_sandboxes:max_por_plantilla`). Cada combinación arranca
daemon y gateway nuevos y deja su log en `res/kindling-parN-jailJ-cuotaC-<modo>-<n>.log`.

## Límites conocidos

- Los percentiles por fase salen de cubos acumulados (interpolación lineal): resolución de los cubos.
- El formato de `/v1/metrics` del gateway se asume Prometheus de texto; si no lo es, la fila del
  gateway sale vacía (con aviso en stderr) y el resto del banco sigue.
- No hay `shellcheck` en el nodo de desarrollo: pasar `shellcheck scripts/bench/*.sh scripts/bench/remoto/*.sh`.
