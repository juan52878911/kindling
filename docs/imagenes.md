# Imágenes sin root: `internal/imagen` y los constructores `debian` y `oci`

Los constructores de siempre (`base`, `mcp`, `llm`) montan un loopback, hacen
chroot y llaman a apt o apk: necesitan root, `mkfs.ext4` y un núcleo que monte
ext4. El constructor `android` ya lo hacía todo en Go; lo que no era de Android
ahora está en `internal/imagen` y lo usa también un constructor nuevo, `debian`.
Con él se construyen imágenes en el daemon de macOS (kling-vz), en un host sin
loop o en un contenedor, y salen iguales bit a bit con las mismas entradas.

## Qué hay en `internal/imagen`

| Pieza | Qué hace |
|---|---|
| `DebianLock` (`debian_lock.go`, de `go run ./internal/imagen/lockgen`) | la base Debian fijada: `debian:trixie-slim` por digest y 31 `.deb` (`iptables procps iproute2 dmsetup ca-certificates` y dependencias, más las actualizaciones de seguridad o del punto de Debian de lo que ya trae la imagen, como `apt-get upgrade`) con su sha256 y la marca de `snapshot.debian.org` del día |
| `PrepareBase` / `Base.Write` | la base: capas OCI + `.deb` encima, como lo dejaría dpkg (`status`, `.list`, md5sums, conffiles), `iptables` → legacy, `ca-certificates.crt`, adelgazada; luego el init (`minimal-init.sh`), con o sin verity, y el ext4 con 32 MiB de holgura |
| `Debs.Install` / `Debs.Ajustar` | lo mismo sobre cualquier árbol: la base o el `/upper` de una capa (partiendo del `status` de la base) |
| `Debs.Resolve` | resuelve paquetes nuevos (Depends y Pre-Depends, sin Recommends) contra los índices de `snapshot.debian.org` del MISMO instante que la base; da un lockfile |
| `Debs.Fetch` / `FetchVerified` | baja y comprueba por sha256; si `deb.debian.org` no lo tiene (o no contesta) va a `snapshot.debian.org`; tres vueltas |
| `PutAgent`, `Entrypoint` | el agente de invitado (comprobado: ELF de la arquitectura) y el `/entrypoint` de `81-base-image.sh` |
| `Verity`, `VerityInit`, `VerityInfo` | árbol de dm-verity y FEC pegados detrás de la capa (`internal/verity`), el bloque del init que la monta por `/dev/mapper` y lo que va a la receta |
| `Marca` | lo que distingue a cada constructor dentro de la imagen: `android` deja la tabla en `/etc/kindling-android/layer.verity` y el dispositivo `android-layer`; `debian`, en `/etc/kindling/layer.verity` y `kindling-layer` |

Android pasa a usarlo sin cambiar lo que sale: `TestBuild` de `internal/android`
construida con el código de antes y el de ahora y comparada con
`go run ./internal/ext4/ext4cmp -mtime` da los mismos ficheros, modos, dueños,
horas y contenidos salvo tres que dependen del puerto aleatorio del registro de
prueba (`IMAGE.txt`, el UUID de `data.ext4` y la raíz de verity), que también
cambian entre dos ejecuciones del código de antes. `lockgen`, con el resolutor
ya en `internal/deb`, saca el mismo conjunto de paquetes (solo cambia openssl,
que tuvo una actualización de seguridad después del día del lock).

## El constructor `debian`

```sh
cat > py.json <<'EOF'
{"packages": ["python3-minimal"], "verity": true}
EOF
kling image build py -builder debian -spec py.json
kling run -name py -image py -allow-exec
kling exec py -- python3 -c 'print(1+1)'
```

El spec:

| Campo | |
|---|---|
| `packages` | paquetes de trixie (hasta 64), con sus dependencias y sin Recommends |
| `lock` | los `.deb` exactos (`name`, `version`, `url`, `sha256`, `size`): el `built.lock` de una construcción anterior. Con él no se baja ningún índice. Las URL solo pueden ser de `deb.debian.org`, `security.debian.org` o `snapshot.debian.org`: el constructor corre como root en el host del daemon |
| `env`, `service` | como en `81-base-image.sh`: variables que el `/entrypoint` carga de `/etc/kling/env` (0600 de root, no legible por el resto del invitado) y un ejecutable que arranca (y relanza) antes de ceder el PID 1 al agente. Sin `service`, solo el agente |
| `verity`, `fec_roots` | la capa detrás de dm-verity, con 2 raíces de FEC por defecto (0 = sin FEC) |
| `arch`, `base_name` | `amd64` o `arm64` (por defecto la del host; para otra, `KLING_GUEST_AGENT_<arch>`) y el nombre de la base (`<nombre>-base`) |

Salen dos ficheros, como con `android`: la base (`debian:trixie-slim` + la base
fijada + el init) y la capa, con los paquetes pedidos, su `status` de dpkg
completo, el agente, el `/entrypoint` y `/etc/kindling/IMAGE.txt`. Los paquetes
van en la capa y no en la base para que verity cubra lo que se pidió. La base es
de la capa (lleva su tabla): solo se sobrescribe una base que escribió este
constructor (`builder: debian-base` en su receta).

La receta lleva en `built` la imagen de Debian, el snapshot, los paquetes de la
base y los añadidos, el `lock` y, con verity, la raíz, la sal y la tabla. La
identidad de la construcción sale de lo que se fija (el lock, no cómo llegó):
resolver o dar el lock de esa resolución da la misma imagen, bit a bit, con
`SOURCE_DATE_EPOCH` igual.

### Medido (2026-10-01)

| Dónde | Qué | Tiempo | Tamaño |
|---|---|---|---|
| lab (CT 105, amd64), daemon privado | `bash` (ya en slim: capa sin paquetes), en frío (base y sus 28 `.deb` incluidos) | 85–108 s | base 129 MiB, capa 26 MiB (el agente) |
| lab | `bash python3-minimal` + verity, en frío | 55–60 s | capa 39 MiB, 4 paquetes, 331 ficheros; verity 81 bloques de hash + 82 de FEC (0,1 s) |
| lab | lo mismo, caché caliente | 4,9 s | |
| Mac M4 (`kling builder debian`, sin daemon ni root), arm64 | `python3-minimal` + verity, en frío | 75 s | base 157 MiB, capa 38 MiB; `e2fsck -fn` limpio |
| Mac M4 | otra vez, caché caliente | 6,5 s | mismos sha256 de base y capa |

Casi todo el tiempo en frío son descargas: en el laboratorio `deb.debian.org`
daba `TLS handshake timeout` en varios `.deb` de cada construcción (15 s cada uno
antes de ir a snapshot). En el laboratorio la imagen sin verity arranca con el núcleo de
Firecracker de siempre y `kling exec` responde (`bash 5.2.37`); la de verity
arranca con el núcleo de Android (`6.1.140-kindling`, `CONFIG_DM_VERITY`):
`dmsetup status` da `kindling-layer: ... verity V` y `python3` (3.13.5) corre.

### Verity en kling-vz (Mac M4, 2026-10-01)

Primera vez en Virtualization.framework. Núcleo: el de
`scripts/builders/kernel` (6.1.140, ahora con `CONFIG_DM_VERITY`), compilado
en arm64; daemon vz propio y la imagen `python3-minimal` de arriba, con FEC
(2 raíces) y sin él (`"fec_roots": 0`).

| Caso | Resultado |
|---|---|
| capa intacta, con y sin FEC | arranca en frío en 157–197 ms; `dmsetup status` da `kindling-layer: 0 79512 verity V`, la capa está montada desde `/dev/mapper/kindling-layer`, el SHA-256 usa `sha256-ce` y `python3` corre |
| un byte cambiado en el superbloque de la capa, sin FEC | `verity: data block 0 is corrupted`, `mount: can't read superblock on /dev/mapper/kindling-layer`, el init sale y el núcleo entra en pánico: el agente no llega a escuchar |
| el mismo byte, con FEC | `verity-fec: FEC 0: corrected 1 errors` y arranca normal (es para lo que está el FEC) |
| capa intacta con el núcleo de Firecracker CI (6.1.177, sin device-mapper) | `refusing to mount the layer unverified` y pánico |

En vz un pánico del invitado no deja la máquina `failed` como en Firecracker:
`panic=1` reinicia y Virtualization.framework vuelve a arrancar el invitado,
así que la máquina sigue `running` y el pánico se repite (7 en un minuto) hasta
que `-wait-ready` se rinde. La capa nunca se monta sin verificar.

## Imágenes de Docker: el constructor `oci`

Una imagen de Docker/OCI tal cual, sin Docker en el host:

```sh
kling image import postgres:17-alpine
kling run -image postgres-17-alpine -mem 512M -wait-ready -e POSTGRES_PASSWORD   # el valor, del entorno
kling save <id> pg-warm && kling run -from pg-warm             # plantilla ya inicializada

kling run -image redis:7-alpine -mem 256M -wait-ready          # o en un paso: una referencia
kling run -image postgres:17-alpine -e POSTGRES_PASSWORD       # se importa la primera vez;
                                                               # una imagen por referencia
```

### El entorno es de la máquina

`kling run -e KEY=valor`, `-e KEY` (el valor sale del entorno de `kling`: no
queda en `ps`) y `-env-file F` dan el entorno a **esa máquina**, no a la
imagen. Dos máquinas con contraseñas distintas son dos máquinas sobre la misma
imagen. Vale para cualquier imagen con un agente que lo sepa leer (las de
ahora). Con una de antes, que lo ignoraría, la máquina **falla** en cuanto su
agente contesta: su servicio habría corrido sin la contraseña o la clave que
se le dio. Hay que reconstruirla o reimportarla. Las importadas antes con
`run -image <ref> -e` llevan la contraseña horneada en `/etc/kling/env`:
bórralas (`kling image rm`).

Cómo viaja, y dónde **no** está:

| Tramo | |
|---|---|
| CLI → daemon | en el cuerpo de `POST /machines` (`env`), nunca en un argv. Si el daemon no anuncia la capacidad `machine-env`, el CLI no lo manda |
| daemon | valida (≤ 256 variables, ≤ 32 KiB, claves `[A-Za-z_][A-Za-z0-9_]*`, sin NUL) y lo escribe en el almacén MMDS del VMM antes de arrancar, con `kling.env=1` en la línea del kernel. No va a `state.json`, ni a un log, ni a `kling inspect`/`ps`: la máquina guarda solo los nombres (`env_keys`). Un error nombra la clave, nunca el valor |
| invitado | `kling-guest` lo lee de MMDS (169.254.169.254, v2 con token) antes de escuchar y lo guarda solo en su memoria, encima del entorno de la imagen (con la misma clave gana la máquina). Lo reciben el servicio, la sonda de listo, los ganchos y `kling exec`. Ningún fichero del invitado lo lleva |
| después | el daemon lo borra de MMDS en cuanto el agente contesta (`/healthz`): un proceso lanzado luego ya no lo encuentra allí. Si `kling.env=1` y el agente no lo pudo leer, el servicio **no arranca** (`kling logs -service` dice por qué) |

Lo que sí queda: el entorno del proceso del servicio (como en Docker, legible
por su usuario y por root en `/proc/<pid>/environ`) y, por tanto, la memoria
de la máquina. Todo lo que vuelca la RAM a disco la lleva: `kling freeze`
(también el de `on_ttl`, que por defecto congela), `kling fork` y `kling save`.
A diferencia de los secretos de sesión (`kling machine secret`, que marcan la
máquina y no dejan congelarla), el entorno se trata como configuración: se
congela con ella, en ficheros 0600 del daemon. **`kling save` congela esa memoria**: una plantilla lleva el
entorno de la máquina de la que se hizo, y sus copias (`run -from`) arrancan
con él (`env_keys` se hereda). Con `-from`, `-e` es un error: la copia es la
memoria del dorado, su servicio ya arrancó con el entorno de aquel, y uno
nuevo no le llegaría (un `POSTGRES_PASSWORD` solo cuenta en el `initdb`). Para
otro entorno, otra máquina en frío con `-image`. La plantilla, como el
`mem.file` de cualquier dorado, es 0600 del daemon.

`kling image import -e` sigue horneando valores en `/etc/kling/env` por
compatibilidad, con un aviso en stderr: sirve para lo que es de la imagen
(`PGDATA`), no para una contraseña.

**La imagen es su propia base.** No va encima de la base de kindling (Alpine):
se aplanan todas sus capas, con sus whiteouts, en un ext4 monolítico. No se
mezclan dos `/lib` (glibc y musl) ni dos `/etc`. Lo que se añade es lo de
kindling y nada más:

| Dónde | Qué |
|---|---|
| `/sbin/overlay-init` | `minimal-init.sh`, con el contrato de runtime de Docker (`/dev/fd`, `/dev/std*`, `/dev/shm`, `/etc/hosts`, nombre) |
| `/usr/local/bin/kling-guest`, `/entrypoint` | el agente de invitado como PID 1 |
| `/etc/kling/env` (0600 de root) | el `Env` de la imagen con el del spec encima |
| `/etc/kindling/service.json` | `ENTRYPOINT`+`CMD` con `USER`, `WORKDIR` y `STOPSIGNAL`: lo arranca y vigila el agente |
| `/etc/kindling/ready` | la sonda de "listo": el `HEALTHCHECK` de la imagen o, sin él, que acepte conexiones el primer puerto TCP de `EXPOSE` (`kling-guest -probe-tcp`) |
| `/etc/kindling/oci.json`, `IMAGE.txt` | la referencia, el digest y la configuración entera |
| `/overlay /rom /proc /sys /dev /run /tmp` | los puntos de montaje que la imagen no traiga (la raíz es de solo lectura) |

**Fijada por digest.** La etiqueta se resuelve una vez: el digest sale del
sha256 del manifiesto bajado (si el registro dice otro en
`Docker-Content-Digest`, error) y queda en la receta (`built.digest`, junto al
del manifiesto de la plataforma y cada capa). Cada capa se comprueba por sha256
y se guarda en la caché por hash (`$KLING_ROOT/cache/builder/oci`, la del
usuario de construcción; `$KLING_ROOT/cache/oci` si corre como root): reimportar el mismo
digest no baja ninguna capa, y con la misma `SOURCE_DATE_EPOCH` sale la misma
imagen bit a bit. De un índice multiplataforma se elige `linux/<arch>` (en arm64,
la variante v8; en amd64, la que no pide v2/v3).

**Rápido sin dejar de verificar.** Las capas se bajan de 4 en 4 y cada una se
descomprime una sola vez, en paralelo y después de comprobar su sha256, a un
tar en el directorio de trabajo de la construcción: el árbol se arma leyendo
solo las cabeceras y el ext4 lee los datos de ahí (cada tar se borra en cuanto
se ha leído). Cuesta en disco, mientras dura, el tamaño descomprimido de las
capas, con tope de 8 veces `max_mb` (una bomba gzip no llena el disco). Un blob
de la caché no se vuelve a hashear en cada import cuando el constructor corre
como root: solo llega a su ruta con un `rename` después de verificarlo, así
que estar ahí, con el tamaño del manifiesto, siendo un fichero regular sin
escritura para grupo ni otros, es estar verificado; si algo de eso falla, se
rehashea entero. Esa caché es del daemon: quien pueda escribir en ella puede
cambiar también las imágenes y los binarios. Con el usuario de construcción
(abajo) la caché es suya, y lo cacheado se rehashea **siempre**: un
constructor comprometido por una imagen no puede envenenar los imports
siguientes cambiando o renombrando un blob. Una capa dañada después en disco la caza además el
CRC32 del gzip al descomprimirla.

**El servicio lo supervisa el agente** (`pkg/guest/service.go`), no un bucle de
shell: lo arranca después de montar los volúmenes, con el usuario de la imagen
(resuelto con su `/etc/passwd`, sin libc), en su propio grupo de procesos, y lo
relanza si muere (de 1 a 30 s de espera). Su salida va a la consola (`kling
logs`) y a `/var/log/kling-service.log`, que es lo que enseña `kling logs
-service` (con `-f` para seguirla); `GET /service` del agente da su estado y la
cola del log.

**Parada limpia.** El agente anuncia la capacidad `service` en `/healthz` solo si
la imagen declara un servicio. A esas máquinas, `kling stop` y `kling rm` les
piden antes `POST /service/stop`: el agente manda la `STOPSIGNAL` de la imagen
(SIGINT en Postgres: "fast shutdown" con su checkpoint) y, pasados 10 s, SIGKILL
al grupo; después el daemon vacía los volúmenes y mata la VM. Las demás no pagan
ese viaje.

**Para agentes.** `kling image import <ref> -json` y `kling run ... -wait-ready
-json` dan lo que hace falta para seguir sin leer texto: el digest, los puertos y
la sonda; el id, la IP y si está lista.

El spec (`kling image build <n> -builder oci -spec s.json`, o `kling image import`):

| Campo | |
|---|---|
| `ref` | `postgres:17-alpine`, `ghcr.io/o/r:tag`, `repo@sha256:...` |
| `digest` | fija la imagen (del índice o del manifiesto); tiene que cuadrar con el de `ref` si trae uno |
| `arch` | `amd64` o `arm64` (por defecto la del host) |
| `env` | `KEY=valor` que se suman al `Env` de la imagen. Van dentro de la imagen y en la receta (0600; `kling image recipe` los enseña como `KEY=***`): para lo que es de cada máquina, `kling run -e` (arriba) |
| `entrypoint`, `cmd`, `user` | sustituyen a los de la imagen, como en `docker run` (`entrypoint` descarta el `CMD`) |
| `max_mb` | tope de lo que se baja, comprimido (4096 por defecto); aplanada no puede pasar de 8 veces eso ni de 2 millones de ficheros |

`kling image import <ref>` es eso con nombre por defecto (`postgres-17-alpine`),
`-e KEY=valor`, `-e KEY` (el valor sale del entorno: no queda en `ps`),
`-env-file` (hornean el valor; avisa y recomienda `run -e`), `-user`, `-entrypoint`, `-max-size`, el comando tras `--` y `-json`
para agentes y scripts (`{name, ref, digest, manifest, arch, ports, volumes,
ready, service, already_imported}`).

El nombre por defecto sale del repositorio y la etiqueta, así que `redis:7` y
`ghcr.io/x/redis:7` dan los dos `redis-7`. Si ya hay una imagen con ese nombre,
`import` no la pisa: si es la misma importación (misma referencia y mismas
opciones) dice `already imported` sin rehacerla; si es otra cosa, falla. Con
`-replace` la reconstruye (y vuelve a resolver la etiqueta).

### Medido (2026-10-01, lab CT 105, amd64, daemon privado)

| Qué | |
|---|---|
| `kling image import postgres:17-alpine` en frío | 11,2 s: 10 capas, 111 MiB comprimidos → ext4 de 331 MiB, 2910 ficheros |
| `kling run -image ... -mem 512M -cpus 2 -wait-ready` | 3,6 s del comando hasta "listo" (arranque 32 ms; initdb, el servidor temporal del entrypoint y el definitivo) |
| El servicio | `docker-entrypoint.sh postgres` original; postgres como uid 70 (`su-exec`); `scram-sha-256` desde el host con la contraseña de `-e`, rechazada la mala |
| `kill -9` del postmaster | relanzado en ~1 s, recuperación del WAL y listo; `GET /service` da `starts: 2`, `last_exit: signal: killed` |
| `kling stop` con Postgres sobre un volumen | servicio parado en 20 ms ("fast shutdown", checkpoint); otra máquina con el mismo volumen arranca con "database system was shut down", sin recuperación, y con los datos |
| `kling save` + `kling run -from` | plantilla de 151 MiB de memoria; instanciar 8–51 ms ya "listo"; `select count(*)` de 100 000 filas desde el host, 60–100 ms con el arranque de psql |
| `kling image import docker.io/timescale/timescaledb:latest-pg16` en frío | 41 s: 18 capas, 575 MiB comprimidos → ext4 de 1825 MiB, 7200 ficheros |
| `kling run -image tsdb -mem 1G -cpus 2 -wait-ready` | 9,1 s hasta "listo" (initdb, `timescaledb-tune` y arranque); `timescaledb` 2.30.2 con licencia `timescale` (TSL), `CREATE EXTENSION vector` → 0.8.1, hypertable de 100 000 filas con 71 chunks comprimidos |
| plantilla de Timescale | 211 MiB de memoria; instanciar 9–57 ms; `count(*)` sobre los chunks comprimidos desde el host, 0,3 s (0,5 s la primera) |

Sin una línea de código por imagen: ni enlaces a mano en `/dev` (lo que hizo
falta en la evaluación del 2026-09-30) ni `chroot`.

### Imágenes grandes: RAM, CPU y disco

Lo que salió de correr Hindsight (memoria de agentes: API en Python, Postgres
embebido y dos modelos locales; 2,6 GiB de imagen, ~1 GiB de proceso) en el
laboratorio, Docker y kindling en el mismo host con 2 CPU:

| | Docker | kindling |
|---|---|---|
| Arranque en frío hasta listo | 17,6 s | 19 s |
| Instancia nueva con sus datos, hasta la primera respuesta | 17,6 s (y sin datos) | **0,55 s** desde la plantilla |
| Respuesta en caliente | 0,33 s | 0,37 s |
| RAM por instancia | 825–925 MiB | **~100 MiB propios** + ~440 MiB compartidos entre todas |
| Puertos en el host | los que se publiquen | ninguno |

Y lo que hubo que cambiar para llegar ahí:

- **CPU.** El techo por defecto del daemon es medio núcleo, pensado para un
  servidor MCP que atiende una llamada cada tanto. Un contenedor de Docker
  corre con los núcleos enteros salvo que se le ponga `--cpus`, y con medio
  núcleo Hindsight tardaba 43 s en arrancar y 1,4 s por consulta. El
  constructor `oci` deja en la receta `cpu_pct_per_vcpu: 100` (un núcleo por
  vCPU); `-cpu-pct` sigue mandando. Hoy el arranque lo cubre también el
  impulso hasta la sonda de listo (todas las vCPU enteras hasta que pasa, ver
  `api.md`), pero la receta se queda por el reposo: con medio núcleo, una
  consulta de Postgres que suma 3 M filas tarda 0,93 s en vez de 0,49 s, y un
  recall de Hindsight 0,09 s en vez de 0,05 s.
- **Disco de la máquina.** Era fijo, de 512 MiB: un modelo de 470 MB no cabía.
  `kling run -disk 4G` lo agranda (de 64 MiB a 256 GiB; disperso, así que solo
  cuesta lo que se escribe). Las copias de una plantilla heredan el del dorado.
- **Apretar antes de volcar.** `save` y `freeze` inflan el globo del invitado
  justo antes de volcar, para que suelte lo que tiene libre y su caché de
  página (en Hindsight, 0,9 GiB de los ficheros del modelo que el proceso ya
  tiene en memoria), y lo desinflan a la línea base: el volcado lleva solo lo
  que está en uso, y es lo que las copias y los thaw faultean. Solo en
  Firecracker; `KLING_SQUEEZE_BEFORE_DUMP=0` lo apaga.
- **Congelado diferencial.** Una copia de un dorado se carga con seguimiento
  de páginas sucias, y al congelarla el VMM vuelca solo lo que escribió desde
  el dorado (en vez de los 1,9 GiB de la RAM entera, 6–8 s por copia). Al
  descongelar se construye base + diff en `mem.full` (un clon en btrfs o XFS,
  una copia dispersa en ext4: ahí el disco lo paga mientras la copia corre) y
  el siguiente freeze funde lo nuevo sobre el diff que había. Un `commit` o un
  `fork` de esa copia (volcados completos) hacen que su siguiente freeze
  vuelque entero. `KLING_DIFF_FREEZE=0` lo apaga.
- **Un volcado que no cabe se rechaza antes de pausar** (la RAM más el mínimo
  de disco de siempre), y uno que falla a medias borra lo que escribió. Antes
  llenaba el disco y dejaba un `mem.file` parcial del tamaño de la RAM.

Medido después (2026-10-06, mismo lab, `/root` en ext4, sin reflink):

| | Antes | Después |
|---|---|---|
| Arranque en frío sin flags (techo de la receta) | 43 s | 18 s |
| Dorado de Hindsight (3 GiB de RAM) | 1896 MiB | 1288 MiB (el invitado devolvió ~1,7 GiB antes del volcado) |
| Congelar una copia | 6–8 s, 1905 MiB | **0,4–0,6 s, 230–350 MiB** |
| Cuatro copias dormidas | 7,6 GiB | 1,1 GiB |
| Despertar hasta el primer recall | 1,75 s | 4,7–5,5 s |
| RAM propia por copia viva | ~100 MiB | ~200 MiB |

La última fila es el precio del apretón: el dorado lleva menos caché de
página, así que cada copia vuelve a leer del disco de la imagen lo que toca y
eso es memoria propia (`KLING_SQUEEZE_BEFORE_DUMP=0` lo cambia por un dorado
más grande).

La penúltima fila era el precio de ext4: sin reflink, despertar copiaba los
1,3 GiB de la base. Ya no: **el almacén de copia al escribir guarda un espejo
de la memoria de cada dorado** (`cow/bases/<dorado>/<clave>.mem`, una copia
por versión del dorado), la copia congela su diff dentro de su directorio del
almacén (`cow/m/<id>/mem.file`) y despertar es clonar el espejo y clonar
encima los tramos del diff (FICLONERANGE), sin mover datos. Con un diff de
cientos de MiB lo que cuesta son los tramos: medido, 24 614 tramos (la mayoría
de una página) a ~40 µs cada uno; ni paralelizarlo ni escribir en vez de
clonar lo mejora en ese Btrfs. Si el almacén no está, no cabe, o el overlay de
la copia no vive en él, todo sigue por el camino de siempre.

| Imagen de Docker (2026-10-06, lab, almacén Btrfs) | Dorado | Congelar una copia | Despertar | Estado comprobado tras dos ciclos |
|---|---|---|---|---|
| `postgres:17-alpine` (512 MiB) | 157 MiB | 0,25–0,5 s, 13–17 MiB | **0,10–0,14 s** | `count(*)` 1000 → 1500 → 2000 |
| `redis:7-alpine` (256 MiB) | 55 MiB | 0,12–0,15 s, 7 MiB | **0,08–0,19 s** | `GET k1`, `INCR n` 1 → 2 |
| `nginx:alpine` (256 MiB) | 52 MiB | 0,11–0,15 s, 6 MiB | **0,09–0,18 s** | fichero servido con 1 → 2 → 3 líneas |
| Hindsight 0.10.2 (3 GiB) | 1288 MiB | 1,5–1,9 s, 250–280 MiB | **1,1–1,5 s** | memorias 64 → 65 → 66 → 67 y recall |

Cuatro copias de Hindsight dormidas cuestan en el almacén 58–70 MiB
exclusivos cada una (la que pasó tres ciclos, 464 MiB), y despertarlas
seguidas tarda 0,5–1,3 s cada una. Lo que no cambia: el supervisor de `kling-guest`
no ve morir al API de una imagen cuyo script de arranque sigue vivo (el de
Hindsight se queda con la interfaz web), y una imagen que llama a Hugging Face
al arrancar se cuelga sin salida a internet hasta que se le pone
`HF_HUB_OFFLINE=1`.

### Límites del constructor `oci`

- **El init es un script de sh**: la imagen tiene que traer `sh`, `mount`,
  `pivot_root`, `mkdir` y `ln` (cualquier Alpine o Debian). Una
  imagen *distroless* se rechaza al construir, igual que una que ya traiga
  `/entrypoint`.
- **Sin dm-verity**: la imagen es la raíz, no una capa.
- **`VOLUME` no crea nada**: sin `-volume`, los datos viven en el disco de la
  máquina (512 MiB; `kling run -disk 4G` lo agranda, y es disperso: solo cuesta
  lo que se escribe). La ruta va en la receta (`built.volumes`). Un volumen de
  kling es un ext4 con `lost+found`: montado justo en el `PGDATA`, `initdb` se
  niega ("directory not empty"), igual que en Docker con un punto de montaje.
  Se monta en el padre (`-volume pgdata:/var/lib/postgresql`) o se fija un
  subdirectorio (`-e PGDATA=/var/lib/postgresql/data/pgdata`).
- **El entorno de una plantilla es el suyo**: `run -from` no admite `-e` (ver
  "El entorno es de la máquina").
- **El `HEALTHCHECK` corre con el `USER` de la imagen** (o el de `-user`), como
  en Docker; si ese usuario no existe en la imagen, la máquina no llega a lista.
- **El constructor `oci` corre como root si no hay usuario de construcción**:
  ver abajo.
- **Sin zstd**: solo capas `tar` y `tar+gzip`.
- **No cambia la licencia**: convertir una imagen no la redistribuye, pero
  tampoco quita sus condiciones (la de Timescale no permite ofrecerla como base
  de datos gestionada). No publiques imágenes convertidas.

### El constructor `oci` no corre como root

Baja de internet y parsea tars que no son de fiar, y no necesita root para
nada. En Linux, con el daemon como root, corre con un usuario propio sin
privilegios: `-build-as` / `KLING_BUILD_AS`, por defecto `kindling-build`.

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin kindling-build
```

Es otro usuario que el de Firecracker (`-run-as`, `kindling`), y el daemon no
acepta el mismo: con un uid compartido, un VMM comprometido reescribiría la
caché de blobs y los builds en curso (y metería su código en las imágenes de
otros), y un constructor comprometido podría mandar señales o `ptrace` a los
VMM vivos y escribir en los volúmenes.

| Qué | Dueño y permisos | Para qué |
| --- | --- | --- |
| `<root>/build/` | root, 0711 | se atraviesa; no se lista ni se escribe |
| `<root>/build/<name>.XXXX/` | `kindling-build`, 0700 | el directorio de trabajo, con `request.json`; la imagen sale en `out/` |
| `<root>/cache/builder/oci/` | `kindling-build`, 0700 | su caché de blobs, aparte de `cache/oci` (la de los constructores que corren como root, `debian` y `android`: root no escribe en un directorio de un usuario sin privilegios ni se fía de lo que deje) |

La primera vez, `cache/builder/oci` enlaza (enlaces duros) los blobs que ya
hubiera en `cache/oci`: siguen siendo de root y de solo lectura para él, se
comprueban por sha256 cada vez que se usan y reimportar lo bajado antes no
baja nada.

El usuario tiene que ser **de sistema y dedicado** (uid ≤ `SYS_UID_MAX`): al
acabar cada construcción el daemon mata todos sus procesos, y con varios
daemons en el mismo host (uno de pruebas junto al de systemd) las
construcciones de ese usuario van en fila con un cerrojo de host
(`/run/kindling-build-<uid>.lock`). La raíz de datos tiene que poder
atravesarla (no bajo `/root`); si no, el daemon lo avisa al arrancar y el
constructor corre como root.

El proceso nace con su uid y su gid, sin grupos suplementarios ni capacidades,
y antes de leer la petición se pone `no_new_privs` y topes: 4096 descriptores,
8 GiB de datos (`RLIMIT_DATA`, el montón de Go), 512 procesos del usuario y sin
volcados de memoria; el plazo de 15 minutos es el de siempre. Del entorno del
daemon solo le llega una lista blanca (proxy, certificados, el agente,
`SOURCE_DATE_EPOCH`): el resto puede llevar secretos.

Lo que deja, el daemon lo comprueba antes de usarlo: `out/<name>.ext4` se abre
sin seguir enlaces y tiene que ser un fichero regular, suyo y con un solo
enlace duro; pasa a root 0644 y se mueve a `images/` (después, root y grupo del
VMM, 0640). `recipe.json` también se lee sin seguir enlaces: uno que apuntara a
la receta 0600 de otra imagen la colaría en ésta. Las construcciones de este
usuario van de una en una, y antes y después el daemon mata cualquier proceso
que quede con su uid: lo que un constructor comprometido dejara en segundo
plano no llega a la siguiente.

Sin usuario (macOS, daemon sin root, `kindling-build` inexistente) corre como
hasta ahora, con el uid del daemon, y el daemon lo avisa al arrancar. `debian`
y `android` también son Go puro, pero leen y escriben bases y recetas de
`images/`: siguen corriendo como root.

## Límites

- **Verity necesita device-mapper en el núcleo del invitado.** El núcleo de
  kindling (`scripts/builders/kernel`) lo trae desde que `config-common` activa
  `CONFIG_DM_VERITY` (y `check-kernel-config.sh` lo exige); el de Android
  también (`prototypes/android/kernel`). El `vmlinux` de Firecracker CI que usan
  hoy el laboratorio y `kling image copy` no lo trae: con él el init no monta la
  capa sin verificar, se para (`dm-verity on /dev/vdc failed; refusing to mount
  the layer unverified`) y la máquina queda `failed` en Firecracker (en vz, en
  bucle de pánicos; ver arriba). Es lo que se quiere, pero hay que saberlo.
- **El constructor `base` no tiene `verity`**: su base (`min`, Alpine) se comparte
  entre capas y no trae `dmsetup`; la tabla de cada capa no tiene dónde ir.
- **No se ejecutan los scripts de los paquetes** (postinst) ni se regenera
  `/etc/ld.so.cache`. Basta para bibliotecas e intérpretes; un paquete que monta
  su configuración en el postinst (crear usuarios, `update-alternatives`) no
  queda igual que con apt.
- **El resolutor no mira versiones**: vale porque índices y base salen del mismo
  snapshot. Por eso el snapshot no se puede elegir en el spec.
- **El índice se baja sin comprobar la firma de `InRelease`** (OpenPGP no está en
  la biblioteca estándar), como `lockgen`: lo que queda fijado y se revisa es el
  sha256 de cada `.deb`, que va en la receta.
