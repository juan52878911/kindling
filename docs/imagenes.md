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

**Pararla y arrancarla otra vez.** `kling stop` conserva el disco de la
máquina y suelta su memoria; `kling start` la arranca en frío sobre ese disco.
Como el daemon no guardó los valores, `start` exige otra vez todas las claves
de `env_keys` (`kling start -e POSTGRES_PASSWORD pg`, o `-env-file`) y, si falta
alguna, dice cuáles en vez de arrancar el servicio sin ellas. Al pararla, el
invitado vacía su caché al disco; al arrancarla, el daemon pasa `e2fsck -p`
por ese disco (no lleva journal) por si se paró a la brava —pausada, o con el
anfitrión caído—, y si queda algo que no sabe arreglar solo no arranca y lo
dice.

`kling image import -e` sigue horneando valores en `/etc/kling/env` por
compatibilidad, con un aviso en stderr: sirve para lo que es de la imagen
(`PGDATA`), no para una contraseña.

**La imagen es su propia base.** No va encima de la base de kindling (Alpine):
se aplanan todas sus capas, con sus whiteouts, en un ext4 monolítico. No se
mezclan dos `/lib` (glibc y musl) ni dos `/etc`. Lo que se añade es lo de
kindling y nada más:

| Dónde | Qué |
|---|---|
| `/sbin/overlay-init` | `minimal-init.sh`, con el contrato de runtime de Docker (`/dev/fd`, `/dev/std*`, `/dev/shm`, `/etc/hosts`, nombre); en una imagen sin `sh` (abajo), un enlace a `kling-guest` |
| `/usr/local/bin/kling-guest`, `/entrypoint` | el agente de invitado como PID 1 (sin `/entrypoint` en una imagen sin `sh`) |
| `/etc/kling/env` (0600 de root) | el `Env` de la imagen con el del spec encima. Un valor de la imagen con saltos de línea se guarda entre comillas y lo leen igual los dos inits; uno con un NUL o un CR (o una clave que no lo es) se deja fuera con un aviso que dice su nombre |
| `/etc/kindling/service.json` | `ENTRYPOINT`+`CMD` con `USER`, `WORKDIR` y `STOPSIGNAL`: lo arranca y vigila el agente. Se relanza si sale con error (`-restart always\|on-failure\|no`, `on-failure` por defecto); un `STOPSIGNAL` que no se entiende pasa a `SIGTERM` con un aviso; un `USER` numérico sin entrada en `/etc/passwd` corre con el grupo 0, como en Docker |
| `/etc/kindling/ready` | la sonda de "listo": el `HEALTHCHECK` de la imagen o, sin él, que acepte conexiones el primer puerto TCP de `EXPOSE` (`kling-guest -probe-tcp`). Su `Timeout` es el plazo de cada ejecución y su `StartPeriod` alarga el impulso de CPU del arranque (en `service.json`, hasta 120 s cada uno); `Interval` y `Retries` no se usan |
| `/etc/kindling/oci.json`, `IMAGE.txt` | la referencia, el digest y la configuración entera; `IMAGE.txt` dice qué init lleva (`init=sh` o `init=go`, también en `built.init` de la receta) |
| `/overlay /rom /proc /sys /dev /run /tmp` | los puntos de montaje que la imagen no traiga (la raíz es de solo lectura) |

`/tmp` y `/run` son los de la imagen, en el disco de la máquina, como en
Docker: lo que trae la imagen ahí (`/run/mysqld` del usuario `mysql` en
`mariadb`) sigue ahí, y lo que se escribe en `/tmp` no gasta RAM del invitado
(ni engorda un `freeze`). En las bases de kindling los dos son `tmpfs`.

Sin `HEALTHCHECK` ni un puerto TCP en `EXPOSE` (una imagen que solo expone
UDP, como un DNS) no hay sonda: `import` lo dice (`ready none: ...`) y
`-wait-ready` espera al agente, no al servicio. Un manifiesto *schema 1* de
Docker (obsoleto desde 2017) se rechaza con un error que lo dice.

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
capas, con tope de 8 veces `max_mb` (una bomba gzip o zstd no llena el disco). Un blob
de la caché no se vuelve a hashear en cada import cuando el constructor corre
como root: solo llega a su ruta con un `rename` después de verificarlo, así
que estar ahí, con el tamaño del manifiesto, siendo un fichero regular sin
escritura para grupo ni otros, es estar verificado; si algo de eso falla, se
rehashea entero. Esa caché es del daemon: quien pueda escribir en ella puede
cambiar también las imágenes y los binarios. Con el usuario de construcción
(abajo) la caché es suya, y lo que saca de ella se rehashea **siempre**: un
constructor comprometido por una imagen no puede envenenar los imports
siguientes cambiando o renombrando un blob. Para no pagarlo en cada import, el
daemon (root) pasa a una **caché verificada** de solo lectura para el
constructor lo que usó una construcción correcta, comprobado por sha256 una vez;
de ahí se lee sin rehashear (abajo). Una capa dañada después en disco la caza además el
CRC32 del gzip (o el xxhash64 del zstd, si lo lleva) al descomprimirla.

**Capas gzip y zstd.** Las capas `tar+gzip` y `tar+zstd` (las de `docker
buildx --output compression=zstd` y las de muchas imágenes nuevas) se leen
igual; cuál es lo deciden sus primeros bytes, no el media type, porque hay
herramientas que suben capas zstd etiquetadas como gzip. El descompresor de
zstd es de kindling (`internal/zstd`, Go sin dependencias, RFC 8878): lee
varios marcos seguidos, comprueba el xxhash64 si el marco lo trae y no admite
diccionarios. Su memoria está acotada por la ventana del marco, que no puede
pasar de 128 MiB (la de `zstd --long`): guarda como mucho la ventana, media
ventana más y un bloque (o el tamaño del marco, si es menor), y lo pide de una
vez cuando pasa de un octavo de eso, no doblando. Una capa de 300 MB con
`zstd -3 --long=27` se descomprime con 232 MiB de RSS máximo (medido en el
M4; antes de pedirlo de una vez, 456 MiB). Para que eso no se multiplique por
los núcleos, de las capas zstd se descomprimen dos a la vez como mucho (las
gzip, tantas como núcleos; cada una pide 32 KiB). Medido en el lab (CT 105, i7-8700T, 2026-10-07) con
`postgres:16-alpine` subida a un registro local con sus capas en zstd (la
grande, 286 MiB, con `-19 --long=27`: ventana de 128 MiB) y otra vez en gzip
`-6`: se importa en 2,8 s frente a 3,0 s, arranca lista en 2 s, y las dos
imágenes tienen el mismo árbol (solo cambian la referencia y el digest de
`/etc/kindling`). Esa capa se descomprime a 215 MB/s, frente a 116 MB/s de
`compress/gzip` y 160 MB/s de `gzip -d`.

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
| `source` | `archive`: los blobs ya están en la caché del daemon (los subió `kling image import -archive`, abajo) y no se usa la red; `digest` (el del manifiesto) es obligatorio y `ref`, opcional |

`kling image import <ref>` es eso con nombre por defecto (`postgres-17-alpine`),
`-e KEY=valor`, `-e KEY` (el valor sale del entorno: no queda en `ps`),
`-env-file` (hornean el valor; avisa y recomienda `run -e`), `-user`, `-entrypoint`, `-max-size`, el comando tras `--` y `-json`
para agentes y scripts (`{name, ref, digest, manifest, arch, ports, volumes,
ready, service, already_imported}`).

El nombre por defecto sale del repositorio y la etiqueta, así que `redis:7` y
`ghcr.io/x/redis:7` dan los dos `redis-7`. Si ya hay una imagen con ese nombre,
`import` no la pisa: si es la misma importación (misma referencia y mismas
opciones) dice `already imported` sin rehacerla; si es otra cosa, falla. Con
`-replace` la reconstruye (y vuelve a resolver la etiqueta), salvo que la use un
dorado o una máquina que no esté parada: entonces 409, como al subirla con
`kling image put`; hay que retirar antes lo que la usa.

### Desde un archivo: `docker save` o un layout OCI

```sh
docker save postgres:17-alpine -o pg.tar
kling image import -archive pg.tar                 # postgres-17-alpine, como desde el registro
kling image import -archive todo.tar -image redis:7  # un archivo con varias imágenes
kling image import -archive ./layout/              # un layout OCI en un directorio
```

Sirve para una imagen que no está en ningún registro (construida en local, en
una red sin salida) y lee los dos formatos de `docker save`: el clásico, hasta
Docker 24 (`manifest.json`, `repositories`, `<id>.json` y `<capa>/layer.tar`), y
el layout OCI de Docker 25 en adelante y de skopeo (`index.json`, `oci-layout`,
`blobs/sha256/`), en un tar o en un directorio, con capas sin comprimir, en
gzip o en zstd (`docker buildx --output type=oci,compression=zstd`). Un tar
comprimido no: hay que descomprimirlo antes (`docker save` sin `| gzip`).

El archivo está en la máquina del CLI y el daemon puede estar en otra (`-host`
por SSH): el CLI lo abre, valida su estructura, elige la imagen (`-image
repo:tag`, o su digest, o el id de `docker images`; sin `-image`, la única) para
la arquitectura del daemon, y sube **solo los blobs que el daemon no tenga**, de
uno en uno y en flujo, sin leerlos enteros a memoria (`PUT /oci/blobs/{digest}`,
[api.md](api.md#imágenes)). El daemon comprueba cada uno con su sha256 antes de
dejarlo en su caché de blobs (`$KLING_ROOT/cache/oci`); después el constructor
`oci` construye desde ahí sin red, comprobando la cadena manifiesto →
configuración → capas → `diff_ids` como con un registro (el usuario de
construcción los lee de esa caché y los rehashea). Si la construcción acaba
bien, el daemon pasa esos blobs a la caché verificada (con un enlace duro, sin
copiar: abajo), así que reconstruir la imagen no vuelve a rehashear sus capas.
Reimportar el mismo archivo no sube nada. Esa caché, como la verificada, es de
root con el grupo del usuario de construcción (0750/0640): una imagen privada
no la lee nadie más del host. Tiene el tope de las del constructor (abajo): una
subida que no cabe ni barriendo lo viejo es un `507`.

`docker save` clásico no trae un manifiesto OCI: su `manifest.json` solo dice
qué fichero es la configuración y cuáles las capas, que van **sin comprimir** y
cuyo sha256 es el `diff_id` de la configuración. El CLI escribe con eso un
manifiesto OCI equivalente (capas `application/vnd.oci.image.layer.v1.tar`, cada
una con su `diff_id` como digest), que es el que se sube y el que fija la
imagen. Por eso su digest no es el del registro, aunque la imagen sea la misma:
para cruzarla con `docker images`, la receta guarda el id (`built.config`, el
digest de la configuración).

La receta dice de dónde vino —`source: archive`, el nombre que traía la imagen
en el archivo y el digest del manifiesto— y **nunca la ruta del archivo** en la
máquina del CLI. Reconstruirla (`-replace`) pide los blobs a la caché: si ya no
están, el error dice que hay que volver a importar el archivo. `-max-size` se
compara con lo que suman las capas tal como van en el archivo (sin comprimir en
un `docker save` clásico).

Lo que se rechaza del archivo antes de subir nada: entradas con nombre absoluto
o con `..`, enlaces (simbólicos o duros) que salgan del archivo, entradas
repetidas, ficheros dispersos, capas comprimidas (gzip o zstd) en un `docker
save` clásico (no serían su `diff_id`), una capa que falte (guardada para otra
plataforma) y un manifiesto o un índice que no sea el de su digest. Un enlace
dentro del archivo se resuelve por nombre en el propio índice del tar (docker
enlaza un `layer.tar` repetido al primero), nunca en el disco; en un directorio
no se sigue ninguno.

Medido el 2026-10-07 en el lab (CT 105, amd64, daemon privado, constructor con
`kindling-build`): `docker save postgres:17-alpine` (Docker 20.10, formato
clásico, 10 capas, 286 MiB sin comprimir) se importa en frío en 2,5 s (subir los
12 blobs por el socket y 1,4 s de construcción) y arranca lista en 34-42 ms de
arranque; reimportarlo no sube nada (10 ms). Un `docker save` con esa imagen y
otra de 12 capas construida encima (con un whiteout) solo sube los 4 blobs
nuevos y la construye en 1,5 s; los compartidos van como enlaces dentro del tar.
Desde el Mac por SSH, el layout OCI de Docker 29 de `redis:7-alpine` (con su
atestación al lado) se sube y construye en 2,2 s, con el mismo digest de
manifiesto que en Docker Hub.

Con la caché de root cerrada y lo subido pasando a la verificada (lab, 2026-10-07,
`docker save postgres:16-alpine`, 283 MiB): el primer import tarda 2,4 s y deja
`cache/oci` vacía y los 13 blobs en la verificada (enlazados, `root:kindling-build`
0640, en 0 s); reconstruirlo (`-replace`) no sube nada ni rehashea, 0,70-0,78 s. Una
cuenta del host que no es root ni del grupo no puede listar `cache/oci` ni leer un
blob de la verificada; `kindling-build` sí. Unas cachés de antes (0755/0644) quedan
en 0750/0640 en el primer uso. Con `daemon.build_cache_max_gib = 1`, importar
`timescale/timescaledb:latest-pg16` (1,8 GiB sin comprimir) se para en la capa que no
cabe con un `507` que nombra el tope. Un layout OCI con dos imágenes de capas zstd
(`-19`, `-3` y `-1`) se importa sin `-image` diciendo los nombres, y con él construye
cada una en 0,1-0,2 s; la de `busybox` (init de `sh`) y otra sin `sh` (init en Go)
arrancan, sirven HTTP y su servicio ve un `ENV` de varias líneas igual que Docker.

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

### Registros privados

Sin credenciales, el constructor pide tokens anónimos de lectura, como `docker
pull` sin `docker login`. Para un registro privado (ghcr.io privado, un
`registry:2` con contraseña, ECR o GCR con token), las credenciales se guardan
**en el daemon**, que es quien construye:

```sh
echo "$GITHUB_TOKEN" | kling registry login ghcr.io -u juan
kling registry login registry.example.com:5000 -u juan     # en una terminal la pide sin eco
aws ecr get-login-password | kling registry login 123.dkr.ecr.eu-west-1.amazonaws.com -u AWS
kling registry import                # las de ~/.docker/config.json (auths con "auth")
kling registry ls                    # registros y usuarios, sin contraseñas
kling registry logout ghcr.io
```

La contraseña o el token se leen de stdin, nunca de un argumento (`ps` y el
historial los verían). `import` solo entiende las que el `config.json` lleva en
claro (`auths.<registro>.auth`, base64 de `usuario:contraseña`); las de un
`credsStore` o un `credHelpers` (el llavero de macOS, `ecr-login`) no están en
el fichero, y lo dice.

Dónde viven y por dónde pasan:

- **En `$KLING_ROOT/registries.json`, root 0600**, en claro (como
  `~/.docker/config.json`): quien lee la raíz de datos ya es root. No en el
  `spec` de la construcción, que va entero a la receta (`kling image recipe`,
  `kling image copy`). `GET /registries` no devuelve nunca la contraseña.
- **El constructor recibe solo las del registro de su referencia**, en
  `registry-auth.json` (0600, suyo) dentro de su directorio de trabajo: lo lee
  y lo borra antes de bajar nada, y el directorio se borra al acabar. Ni por
  argv ni por el entorno (el de un constructor sin root es de lista blanca, y
  el de un proceso se lee en `/proc/<pid>/environ`). No salen en el log de la
  construcción, en la receta, en `state.json` ni en los eventos.
- **Solo hacia su registro**: Basic directo si el registro lo pide (un 401 con
  `Basic`: `registry:2` con htpasswd, ECR), o el flujo Bearer: el token se pide
  con Basic al servicio que indica el 401, que tiene que ser https y estar en
  el dominio del registro (`auth.docker.io` para Docker Hub, `gitlab.com` para
  `registry.gitlab.com`); a uno de otro dominio no se le mandan y el import
  falla diciéndolo. Tras una redirección a otro host (las capas suelen ir a un
  CDN con la URL ya firmada) la cabecera `Authorization` se quita, también
  hacia otro puerto del mismo host o un subdominio, que Go sí dejaría pasar.
  Siempre por https, salvo `localhost`/`127.0.0.1`/`::1`.
- **Sin ellas, o rechazadas, el error lo dice**: `kling registry login
  <registro>` si no hay, o que las guardadas se rechazaron.

No se comprueban al guardarlas: el primer import desde el registro es la
prueba. Un token que caduca (ECR, 12 h; GCR, 1 h) hay que volver a guardarlo.

Las capas que se bajan con ellas quedan en las cachés de root, que no lee
ninguna otra cuenta del host (directorios 0750 y blobs 0640 del grupo del
usuario de construcción; ver [abajo](#el-constructor-oci-no-corre-como-root)).

Comprobado el 2026-10-07 en el lab (CT 105, amd64, daemon privado, constructor
con `kindling-build`) contra un `registry:2` con htpasswd en `127.0.0.1:5093`,
con imágenes subidas con `docker push`: sin credenciales el import falla con el
401 y la pista `kling registry login 127.0.0.1:5093`; `kling registry login -u
<usuario>` con la contraseña por stdin lo arregla, `ls` y `GET /registries` dan
solo registro y usuario, `logout` devuelve el 401 y `kling registry import`
toma las de `~/.docker/config.json`. La contraseña (y su `usuario:contraseña`
en base64) no aparece en ningún fichero de la raíz de datos salvo
`registries.json` (root 0600): ni en la receta, `state.json`, las cachés o
`build/`, ni en el log del daemon ni en su directorio de `/run`; tampoco en
`/proc/*/cmdline` ni `/proc/*/environ`, muestreados cada 20 ms durante un
import. Una imagen con capas zstd hechas a mano (las de `nginx:alpine`
recomprimidas con zstd 1.5.7 a `-19`, `-1 --no-check`, `-3` y la grande en dos
marcos, uno `-3 --long=27`) subida con un manifiesto OCI a ese registro se
importa en 0,69 s y nginx responde, del mismo tamaño que con las capas gzip.

### Límites del constructor `oci`

- **Dos inits, el mismo contrato.** Si la imagen trae `sh`, `mount`,
  `pivot_root`, `mkdir` y `ln` (cualquier Alpine o Debian), el init es
  `minimal-init.sh`, como en las bases de kindling. Si le falta alguno (una
  *distroless*, una `scratch` con un binario estático, `traefik/whoami`), o si
  la imagen ya trae su propio `/entrypoint`, el init es el de Go de
  `kling-guest` (`/sbin/overlay-init` es un enlace a él): hace lo mismo que el
  script (overlay, `pivot_root`, `/proc`, `/sys`, `/dev`, `/dev/fd`,
  `/dev/shm`, nombre, `/etc/hosts` sin pisar lo que haya), carga
  `/etc/kling/env` (como sh: un valor con saltos de línea sigue entre
  comillas en las líneas siguientes) y se vuelve a ejecutar como agente, sin
  `/entrypoint` y sin ningún programa de la imagen. Solo arranca como PID 1.
  El script se queda para las imágenes que lo pueden correr porque es el que
  llevan todas las importadas hasta ahora; los dos se prueban con los mismos
  casos. Comprobado el 2026-10-07 en el lab (CT 105, amd64, daemon privado,
  constructor con `kindling-build`): `gcr.io/distroless/static-debian12` con
  un servidor HTTP en Go (subida a un `registry:2` local) y `traefik/whoami`
  salen con el init de Go, quedan listas y responden por HTTP (`:9090` y
  `:80`); `run -e FOO=bar` llega al servicio por MMDS. Por el camino del
  script, `postgres:17-alpine`, `nginx:alpine` y `mariadb:11` se importan en
  6,1, 2,3 y 5,9 s, quedan listas y responden (`psql`, la página de nginx,
  `select version()` de MariaDB 11.8.9); el nombre del invitado es `kindling`
  con los dos inits. En una imagen sin el comando `ip` (las dos de Go), el
  agente avisa de que no puede poner la ruta a 169.254.169.254, sin
  consecuencias: MMDS responde igual por la ruta por defecto.
- **Sondas sin shell.** En una imagen sin `/bin/sh` la sonda de listo no es un
  script: es un `#!` que apunta al agente (`kling-guest -probe-tcp=...` para
  el puerto de `EXPOSE`, `kling-guest -exec-json` con el argv de un
  `HEALTHCHECK CMD` en la segunda línea), que el kernel ejecuta sin shell. Un
  `HEALTHCHECK CMD-SHELL` necesita `sh` (en Docker tampoco pasaría nunca): se
  ignora con un aviso y queda la sonda de `EXPOSE`.
- **El agente escucha en el 8080 del invitado**: un servicio que quiera ese
  puerto no arranca (`address already in use` en `kling logs`).
- **Sin dm-verity**: la imagen es la raíz, no una capa.
- **`VOLUME` no crea nada**: sin `-volume`, los datos viven en el disco de la
  máquina (512 MiB; `kling run -disk 4G` lo agranda, y es disperso: solo cuesta
  lo que se escribe). La ruta va en la receta (`built.volumes`).
- **Un volumen nuevo hereda el directorio de la imagen**, como en Docker: la
  primera vez que se monta, si no tiene más que el `lost+found` de `mke2fs`,
  su raíz toma el dueño y el modo del directorio que la imagen tiene en ese
  punto, y se copia lo que haya dentro (sin seguir enlaces, con dueños, modos y
  fechas) si no pasa de 64 MiB ni de 65 536 entradas; si pasa, solo el dueño y
  el modo. Así un servicio sin root (`grafana`: `USER 472`, `VOLUME
  /var/lib/grafana`) puede escribir en él. Un volumen con algo más dentro no se
  toca nunca, ni uno de solo lectura; los atributos extendidos no se copian.
  Se hace una sola vez: queda una marca en `lost+found/.kling-seeded`, así que
  un `chmod` en la raíz de un volumen aún vacío no se deshace al reiniciar, y
  los arranques siguientes no vuelven a montar ni a recorrer nada. Una copia
  que falla no se marca y se reintenta en el siguiente arranque.
- **El `lost+found` sigue ahí**: montado justo en el `PGDATA`, `initdb` se
  niega ("directory not empty"), igual que en Docker con un punto de montaje.
  Se monta en el padre (`-volume pgdata:/var/lib/postgresql`) o se fija un
  subdirectorio (`-e PGDATA=/var/lib/postgresql/data/pgdata`).
- **El entorno de una plantilla es el suyo**: `run -from` no admite `-e` (ver
  "El entorno es de la máquina").
- **El `HEALTHCHECK` corre con el `USER` de la imagen** (o el de `-user`), como
  en Docker; si ese usuario no existe en la imagen, la máquina no llega a lista.
- **El constructor `oci` corre como root si no hay usuario de construcción**:
  ver abajo.
- **zstd con ventana de hasta 128 MiB**: una capa de `zstd --long=28` o más
  se rechaza con un error que lo dice; las de los niveles normales (hasta
  `--ultra -22`) y `--long` caben. Sin diccionarios.
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
| `<root>/build/<name>.XXXX/` | `kindling-build`, 0700 | el directorio de trabajo, con `request.json` y, para un registro privado, `registry-auth.json` (0600, lo borra al leerlo); la imagen sale en `out/` |
| `<root>/cache/builder/oci/` | `kindling-build`, 0700 | su caché de blobs, aparte de `cache/oci` (la de los constructores que corren como root, `debian` y `android`: root no escribe en un directorio de un usuario sin privilegios ni se fía de lo que deje) |
| `<root>/cache/oci/sha256/` | root, grupo de `kindling-build`; directorios 0750, ficheros 0640 | la caché de blobs de root: lo subido por `image import -archive` (lo lee y lo rehashea) y lo de los constructores que corren como root. Sin usuario de construcción, 0700/0600 |
| `<root>/cache/verified/oci/sha256/` | root, grupo de `kindling-build`; directorios 0750, ficheros 0640 | la caché verificada: la lee sin rehashear, no puede escribir, renombrar ni borrar nada |

Ninguna de las cachés de root deja leer, listar ni atravesar a las demás
cuentas del host: las capas de una imagen privada (`kling registry login`) o de
un archivo son tan privadas como `images/` (0750). Lo que dejaron versiones
anteriores (0755/0644) se cierra la primera vez que se usa cada caché.

La primera vez, `cache/builder/oci` enlaza (enlaces duros) los blobs que ya
hubiera en `cache/oci`: siguen siendo de root y de solo lectura para él, se
comprueban por sha256 hasta que pasan a la verificada y reimportar lo bajado
antes no baja nada.

**La caché verificada.** Lo que el constructor saca de su caché se rehashea
cada vez (1-3 s por GiB). Por eso, al acabar **bien** una construcción, con sus
procesos ya barridos y el cerrojo de host aún tomado, el daemon lee la lista de
blobs que usó (`<trabajo>/cache-used`, que deja el constructor; solo cuentan las
líneas `sha256:<64 hex>`) y cada uno que aún no esté en la verificada lo **copia**
desde `cache/builder/oci` a un fichero nuevo de root, hasheando lo que copia:
entra solo si el sha256 es el de su nombre, con un `rename` después del
`fsync`. El original sale de la caché del constructor; uno que no cuadra se
borra y no entra. Copiar, y no mover ni enlazar, es a propósito: el inodo nace
de root, sin ACL, sin otros enlaces duros ni descriptores del constructor, y lo
hasheado es exactamente lo que queda. No se sigue ningún enlace (ni en la lista
ni en los blobs ni en los directorios de su caché); un blob tiene que ser
regular y suyo, o de root y legible por él (los enlazados de `cache/oci`, de su
grupo y 0640). Lo que usó y no estaba en su caché sino en `cache/oci` (lo subido
de un archivo, que lee como `Seed`) pasa con un **enlace duro** y sale de allí:
esa caché solo la escribe root, y cada blob entró comprobado por sha256 (la
subida, o una descarga como root).
Su caché se abre una vez como `os.Root` y todo (listar, abrir, borrar) va
relativo a ese directorio: aunque cambiara `cache/oci` por un enlace a `/etc`
después de mirarlo, no se sale de ella. Si el barrido de sus procesos no acaba
limpio (alguno sigue volviendo), su caché no se toca: ni se promueve ni se barre.
Tampoco llena el disco del host a través de root: un fichero disperso no se
copia, y en una pasada no se copian más bytes que `daemon.build_cache_max_gib`;
lo que no cabe se queda en la suya, sin verificar.

El cliente OCI del constructor (`KLING_VERIFIED_CACHE_DIR`) mira primero ahí y
usa un blob sin rehashear solo si el fichero, `sha256/`, `oci/` y `verified/`
son de root sin escritura para grupo ni otros, sin enlaces, y el fichero es
regular con el tamaño del manifiesto; si no, como si no estuviera: se baja a su
caché y se rehashea como antes. Lo suyo se sigue rehasheando siempre.

Medido en el lab (amd64, caché caliente): el `Pull` de `postgres:17-alpine`
(111 MiB comprimidos) pasa de 0,27 s rehasheando a menos de 1 ms desde la
verificada, y el de `timescale/timescaledb:latest-pg16` (575 MiB) de 0,78 s a
menos de 1 ms. En el import entero se nota menos, porque el rehash va en
paralelo con la descompresión: `postgres:17-alpine` baja de 3,6 a 3,3 s (mediana
de 6); en timescaledb (unos 10 s) queda dentro del ruido. La primera
construcción que verifica paga una copia: 0,4 s y 2 s.

**Las cachés tienen tope.** Después de cada construcción el daemon barre las
tres (la del constructor, `cache/oci` y la verificada): fuera los `.part` y lo
que no es un blob, lo que lleva más de `daemon.build_cache_max_days` días sin
usarse (30 por defecto; cada uso pone la fecha a un blob verificado o de
`cache/oci`) y, si entre todas pasan de `daemon.build_cache_max_gib` (20 por
defecto), lo más viejo, **primero lo no verificado** (su fecha la pone el
constructor), luego lo de `cache/oci` y al final lo verificado. En `cache/oci`
no se borra nada de menos de 2 horas (una subida que espera a su
construcción, una descarga a medias), y no se barre mientras otra construcción
o una subida la está usando: se deja para la siguiente. Lo que acaba de usar la
construcción no se toca mientras quepa en el tope: la lista la escribe el
constructor, y uno comprometido no mantiene así la verificada por encima de él.
Como mucho 1048576 GiB y 36500 días (más desbordaría); por entorno, un valor
mayor se avisa y queda el de por defecto. `KLING_BUILD_CACHE_MAX_GIB` y
`KLING_BUILD_CACHE_MAX_DAYS` mandan sobre el fichero; se leen en cada
construcción y `GET /info` (y `kling doctor`) dice los efectivos. Una subida
(`PUT /oci/blobs`) que haría pasar a `cache/oci` del tope barre antes lo viejo y,
si aun así no cabe, se rechaza (`507`).

```sh
kling config set daemon.build_cache_max_gib 50
kling config set daemon.build_cache_max_days 7
```

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
