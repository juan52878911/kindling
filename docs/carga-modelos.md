# Carga de modelos: cómo se mide

Cómo mide kindling lo que cuesta cargar y servir un modelo de IA (un dorado de `kling ai model add` o
`kling ai model deploy`): qué corre el banco, qué deja y qué comprueba antes de dar una cifra por buena.
Las cifras de referencia son las de la EC2 anidada de la ronda 8 (c8i.4xlarge, gp3 de 125 MiB/s); las de
la ronda 9 se apuntan aquí cuando se corran.

## Fases de un modelo

| Fase | Qué la limita (ronda 8) | Referencia |
|---|---|---|
| `model add` entero (Q4, 1065 MiB de pesos) | base glibc, descarga, capa, dorado | 105 s |
| Carga de pesos en el dorado («server ready») | limitador de 128 MiB/s del disco | 7,7 s |
| Commit del dorado | tres pases de disco sobre el `mem.file` | ~27 s (una vez 3 min 24 s) |
| Restaurar una réplica | spawn + jail + KVM | 75 ms |
| Primera petición con la caché caliente | fallos de EPT (~261 000 páginas de 4 KiB) | 900 ms |
| Primera petición con la caché fría | caudal del gp3 | 7,4 s |
| Gateway ocioso → primera respuesta (idle remove) | restaurar + EPT | 995 ms |

## El banco

    BINS_DIR=… scripts/bench/ec2.sh --dry-run ia     # plan, coste y llamadas, sin lanzar nada
    BINS_DIR=… scripts/bench/ec2.sh ia               # spot, apagado automático, tope de gasto

En un host Linux con kindling: `USAR_DAEMON=1 JOURNAL_UNIT=<unidad del daemon> bash
scripts/bench/remoto/ia.sh medir`. Las tablas las saca `scripts/bench/ia.py res/ia-*.json`.

### Qué guarda cada corrida

- `journal-<unidad>.log`: el log del daemon de toda la corrida. Su línea `commit X -> n: overlay …, dump …,
  holes …, sync …, digest … (total …)` es el desglose del commit; `ia.sh` la convierte en
  `corrida.json` → `dorados[].commit`. Es lo que hace falta para explicar un commit de 40 s frente a otro de
  3 min 24 s del mismo dorado.
- `dorado-<n>/`: mientras vive la plantilla, cada `VIGILAR_S` s, su `service.log` (por `kling logs
  -service` o `kling exec`), su consola, el `/proc/meminfo` del invitado y el RSS/pico de su VMM. Si el
  dorado falla (el Q8 que «no carga»), queda lo último que se vio, el número de veces que el servicio se
  relanzó (`service exited with …`) y el pico de memoria: con eso se distingue un servidor que muere en
  bucle de uno que carga despacio.
- `corrida.json`: además, los bytes leídos y escritos por disco en cada fase (`/proc/diskstats`), el disco
  de la corrida (gp3 con su caudal o NVMe local) y el resultado de cada spike.

### Muestras calientes de verdad

En la ronda 8 la mitad de las muestras «calientes» leían 1 GiB de disco: venían detrás de un
`drop_caches`. Ahora las calientes van todas antes que las frías y, antes de cada una, el dorado se trae a
la caché (la API `POST /snapshots/<n>/warm` si el daemon la tiene, los ficheros bajo `KLING_ROOT` y una
restauración descartada con su petición). Una muestra cuyo VMM aun así lee del disco se aparta
(`primera.calientes_con_disco`) y se repite. `ia.py` rechaza la corrida si alguna muestra caliente leyó
más de `UMBRAL_CALIENTE_BYTES` (0).

### Filas

| Fila | Sección de `96-von-bench.sh` | Qué mide |
|---|---|---|
| Restaurar, caliente / frío | `primera` | primera respuesta tras `run -from` y tras `thaw` |
| Gateway ocioso: freeze, remove, **con repuesto** | `ociosa` | primera respuesta tras dormir; `spare: 1` y el PSS que añade el repuesto |
| **Tras arrancar el gateway** | `arranque` | con la caché fría: en cuanto contesta, tras `ARRANQUE_ESPERA_S` (precalentado al arrancar) y con `pin: true` |
| **Secreto MMDS** | `secreto` (kind http) | coste del gancho tras restaurar (p50 con secreto − sin él) y que el valor no esté en el `mem.file` ni en el log del gateway |
| P3 chispa, variantes `claves`, `abierto`, **`l3`** | `chispa-p3.sh` | `l3`: chispa-server → proxy de credenciales con `upstream` → vLLM de mentira, p50/p95 frente al mismo binario directo; la clave llega al vLLM y no está en el dorado |
| Pico de memoria al fabricar | `ia.sh vigilar` | lo que pide de verdad el servicio, para dimensionar `-mem` (chispa-server se fabrica con 3072 MiB y en el host ocupa ~2 GiB) |

### Disco

La misma corrida con otro disco aísla lo que es caudal: `EC2_DISCO_MIBS=1000 EC2_DISCO_IOPS=4000` (gp3 a
1000 MiB/s) o `EC2_NVME=1` con un tipo que tenga NVMe local (la raíz del daemon del banco va al NVMe con
el mismo sistema de ficheros que la raíz, para no medir reflink a la vez). La cuenta tiene 16 vCPU bajo
demanda y menos de 48 en spot: `ec2.sh` se niega a tipos mayores (`EC2_MAX_VCPU`) y a `.metal`.

## Spikes (ronda 9)

Prototipos fuera del producto, en `prototypes/`, cada uno con su README (hipótesis, cómo se corre,
resultado) y un `spike.sh` que deja `resultado.json` con una recomendación (seguir / descartar):

| Spike | Hipótesis | `IA_PRUEBAS` |
|---|---|---|
| `r9-hugepages` | con el `mem.file` en un tmpfs `huge=always`, KVM mapea los pesos a 2 MiB y la primera pasada tras restaurar baja a una fracción | `s-huge` |
| `r9-volcado-tmpfs` | volcar a tmpfs y pasar al disco con una copia dispersa que hashea a la vez escribe una vez lo asignado: commit ~27 → ~11 s | `s-volcado` |
| `r9-pmem-dax` | con los pesos por virtio-pmem + DAX fuera de la RAM del invitado, el volcado escribe 6× menos; primero comprueba que Firecracker lo admite | `s-pmem` |

Los tres usan una microVM de Firecracker hecha a mano (`prototypes/r9-hugepages/lib-fc.sh`), con un
programa de prueba (`tocar.c`) que recorre los pesos página a página; dan `peticion_ms`, salidas de VM y
páginas de 4 KiB / 2 MiB de la EPT (debugfs de KVM) y bytes escritos.

## Resultados

Pendientes: la línea base con el banco arreglado se corre antes de fusionar las zonas Z1-Z6 de la ronda 9,
y otra vez después, en la misma instancia y con el mismo disco.
