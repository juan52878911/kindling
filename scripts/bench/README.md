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

## Barrido

`PARALELO="2 4 8 16"` (`KLING_MAX_PARALLEL_BOOT`), `JAILER="0 1"`, `CUOTAS_LISTA="- 200:50"`
(`-` = inquilino sin cuotas; `a:b` = `max_sandboxes:max_por_plantilla`). Cada combinación arranca
daemon y gateway nuevos y deja su log en `res/kindling-parN-jailJ-cuotaC-<modo>-<n>.log`.

## Límites conocidos

- Los percentiles por fase salen de cubos acumulados (interpolación lineal): resolución de los cubos.
- El formato de `/v1/metrics` del gateway se asume Prometheus de texto; si no lo es, la fila del
  gateway sale vacía (con aviso en stderr) y el resto del banco sigue.
- No hay `shellcheck` en el nodo de desarrollo: pasar `shellcheck scripts/bench/*.sh scripts/bench/remoto/*.sh`.
