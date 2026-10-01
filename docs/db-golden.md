# Postgres dorado: una base caliente congelada como plantilla

`kling db` dará a agentes y tests bases de datos Postgres desechables. Su primera
pieza (nodo N1) es esta: una microVM con Postgres **ya arrancado, cargado y
caliente**, guardada como plantilla (`kling save`). Cada `kling run -from <plantilla>`
devuelve una copia con sus propios datos en milisegundos, sin `initdb`, sin migraciones
y sin arrancar el postmaster: el proceso sigue donde se congeló.

Aquí solo está la base de datos y su método. La credencial que el invitado nunca ve
(el proxy de [postgres.md](postgres.md)) y el frontal `kling db` son los nodos
siguientes.

## Qué hay

| Pieza | Para qué |
|---|---|
| `scripts/recipes/pg16.recipe.json` | receta de la imagen `pg16`: Alpine + `postgresql16` + `postgresql16-contrib` |
| `scripts/db-golden.sh image` | construye la imagen con el constructor `base` del núcleo |
| `scripts/db-golden.sh build` | arranca, inicializa, carga, hace CHECKPOINT y guarda la plantilla |
| `scripts/db-image-ext.sh` (`kling db golden image -ext`) | la plantilla `pg16-ext` (o `pg17-ext`): Postgres con TimescaleDB (TSL), pgvector, PostGIS, pg_cron y pg_partman compiladas |
| `scripts/db-post-thaw.sh` | paso tras instanciar una copia: espera, comprueba el reloj y avisa |
| `scripts/db-golden-verify.sh` | las comprobaciones de las copias, para correr en el lab |

No hay código Go nuevo: todo usa `kling image build`, `run`, `exec`, `cp` y `save`.

## Construir la imagen

En un host Linux con el daemon (el mismo donde se instalaron `make deploy` y la imagen
base `min`):

```sh
sudo systemctl is-active kindling            # el daemon, en marcha
kling image ls | grep -w min                 # la base Alpine existe
scripts/db-golden.sh image                   # = kling image build pg16 -builder base -base min -grow 512 -spec scripts/recipes/pg16.recipe.json
kling image recipe pg16                      # muestra los paquetes con los que se hizo
```

El constructor `base` (`scripts/81-base-image.sh`) instala los paquetes en una capa
sobre `min` y mete `kling-guest` como PID 1: es el mismo mecanismo que la imagen
`toolchain` (`kling image toolchain`). Necesita salida a internet **en el host**, no
en la microVM. Si `apk` no encuentra `postgresql16`, la Alpine de `min` es demasiado
nueva o vieja para esa versión: `apk search postgresql` dentro de la base dice cuáles
hay, y basta cambiar los nombres en la receta.

## Extensiones que Alpine no trae: la plantilla `pg16-ext`

La imagen `pg16` es Alpine con `postgresql16-contrib` (uuid-ossp, pgcrypto, pg_trgm,
hstore...). Alpine no tiene TimescaleDB ni pgvector para PG16 o PG17 (solo para PG18), y
su TimescaleDB es la build Apache, sin compresión: `add_compression_policy` falla con
"not supported under the current apache license". `scripts/db-image-ext.sh` compila las
extensiones contra el Postgres de la imagen y deja una **plantilla** de la que sale el
golden (`-from`):

```sh
kling db golden image -ext                       # pg16-ext: las cinco
kling db golden image -ext -pg 17                # pg17-ext
kling db golden image -ext -only timescaledb,vector
kling db golden build -from pg16-ext -as-super -migrations init/ <golden>
```

| Extensión | Versión | Notas |
|---|---|---|
| timescaledb | 2.30.2 | licencia TSL: compresión (`add_compression_policy`), agregados continuos; precargada |
| vector (pgvector) | 0.8.0 | sin `-march=native`: el binario vale en cualquier CPU |
| postgis | 3.6.4 | sin raster (GDAL), topology ni protobuf (`ST_AsMVT`) |
| pg_cron | 1.6.8 | necesita precargarse |
| pg_partman | 5.5.0 | |

Además lleva `bash` (para scripts de init al estilo Docker) y la lista de extensiones en
`/usr/share/kling-db-extensions`.

Cómo:

- Una microVM de construcción con salida a internet y un volumen de 3 GiB (el overlay de
  512 MiB no cabe la toolchain). La toolchain va en una raíz `apk --root` del volumen,
  con las firmas comprobadas con las claves de la imagen y reintentos (un índice que no
  bajó entero da "no such package" de paquetes que existen).
- Cada fuente se clona por su etiqueta **y se comprueba por commit**: una etiqueta
  movida en el origen no cuela otro código.
- La compilación corre desacoplada en el invitado (`setsid`) y el host la sondea con
  `exec` cortos: en el laboratorio, una conexión de `exec` de minutos sin salida se caía
  a media compilación. El `make -j4` de PostGIS tiene carreras al generar sus scripts:
  si falla, se acaba en serie.
- No se instala ni la documentación, ni los símbolos de depuración, ni los scripts de
  **actualización** entre versiones (`timescaledb--2.x--2.30.2.sql`...: solo sirven para
  `ALTER EXTENSION UPDATE` desde una versión que la imagen nunca tuvo, y eran 50 MiB).
- La plantilla **no tiene red** (sus copias heredan el egress): las librerías de PostGIS
  se bajan como `.apk` en la de construcción y se instalan sin red, con la firma
  comprobada. Lo de paso va a un tmpfs, se vacía la caché y se aprieta el globo antes de
  guardarla: así su volcado y su disco son solo lo instalado.
- La comprobación se hace en una **instancia de usar y tirar** de la plantilla ya
  guardada: un cluster en un tmpfs, `CREATE EXTENSION` de cada una y una hypertable con
  `add_compression_policy`. Si falla, la plantilla se borra.

### Medidas de pg16-ext

Laboratorio (CT 105: i7-8700T, 4 vCPU, 8 GiB), 2026-10-01:

| | |
|---|---|
| Construcción completa | 466 s (TimescaleDB ~4,5 min; PostGIS ~2,5 min; pgvector, pg_cron y pg_partman, segundos); `pg17-ext`, 477 s |
| Plantilla `pg16-ext` | **50 MiB de memoria, 104 MiB de disco** (con temporales en el overlay, Postgres arrancado dentro y los scripts de actualización: 350 y 475 MiB) |
| Plantilla `pg17-ext` | 50 MiB de memoria, 109 MiB de disco |
| Compilado | 5,4 MiB de extensiones y 14 MiB de librerías de ejecución (`.apk`) |

## Construir el dorado

```sh
scripts/db-golden.sh build -migrations ./db/migrations -seed ./db/seed.sql minidb
scripts/db-golden.sh build -seed-mb 100 pesada          # 100 MiB sintéticos (generate_series)
KLING="kling -H ssh://lab" scripts/db-golden.sh build -seed-mb 20 prueba
```

`-migrations` aplica los `*.sql` del directorio en orden alfabético y `-seed` un SQL de
datos después; `-seed-mb N` los sustituye por una tabla `seed_events` de ~N MiB.
Corren como el rol de la aplicación (así los objetos son suyos); `-as-super` los corre
como superusuario, para migraciones con `CREATE EXTENSION` de extensiones que no son
"trusted".

Qué hace, en orden:

1. arranca `<nombre>-build` desde `pg16` con `-egress none -allow-exec`;
2. `initdb` con `PGDATA=/var/lib/postgresql/data`, **en el overlay de la máquina**;
3. escribe `pg_hba.conf` (superusuario `postgres` solo por el socket local y como
   usuario del sistema; el rol de la app por red, solo con `scram-sha-256`) y arranca
   Postgres escuchando en la IP del invitado (`172.16.0.2`, la misma en todas las
   máquinas, leída de `/proc/cmdline`);
4. crea el rol y la base. **La contraseña se genera en el host** (192 bits), se guarda
   en `~/.local/state/kling-db/<nombre>/password` (0600, directorio 0700; `-state` o
   `KLING_DB_STATE` la cambian) y viaja al invitado **por stdin**: no aparece en argv,
   ni en pantalla, ni en el log de Postgres (`log_min_error_statement = panic`). Si ese
   paso falla, la salida no se muestra a propósito: el error de psql cita la sentencia;
5. aplica migraciones y seed, `VACUUM (ANALYZE)` (en la base y en `postgres` y `template1`, para que el autovacuum no lo haga en cada copia) y `CHECKPOINT`;
6. comprueba que no queda ningún cliente conectado y que el overlay no pasa del 85 %;
7. borra los SQL de entrada del invitado, `sync`, y `kling save -replace` con la
   máquina **viva**;
8. retira la máquina de preparación (`-keep` la conserva si terminó bien).

Es idempotente: repetirlo con el mismo nombre retira la máquina de preparación
anterior y reemplaza la plantilla (un rebuild genera **otra** contraseña). Si algo
falla, un `trap` retira la máquina y borra los temporales; el `password` anterior no se
toca hasta que la plantilla nueva se guardó. Un `kling save -replace` sobre una plantilla
con instancias vivas lo rechaza el núcleo: borra o para las copias antes.

## Por qué PGDATA va en el overlay y no en un volumen

Dos reglas del núcleo lo deciden, y las dos están en el código:

- `Freeze` desmonta los volúmenes (`releaseVolumes`) antes de pausar: con PGDATA en un
  volumen, el postmaster tendría los ficheros de datos desmontados debajo.
- `Fork` (y `sandbox fork`) rechaza las máquinas con un volumen montado en escritura
  (`puedeRamificarse`): solo una máquina a la vez puede escribir en un volumen.
- `Fork` rechaza también las máquinas con credenciales del proxy
  (`forkSinCredenciales`): las copias despertarían con marcadores que su proxy no
  conoce. Para N máquinas con credencial, átala a la plantilla (`kling template
  credential`) y arranca cada una con `run -from`.

En el overlay, cada copia lleva su propia copia del disco de la máquina, tomada con la
memoria en el mismo instante: son consistentes entre sí.

## Qué pasa al despertar una copia con Postgres vivo

Cada copia despierta con la **misma memoria**: el mismo postmaster, con los mismos
`shared_buffers`, PIDs y semillas, y los mismos ficheros en su overlay. El núcleo corrige
lo que está a su alcance:

| Qué | Cómo queda |
|---|---|
| Reloj de pared del invitado | Se corrige solo. `resyncGuest` manda la hora del host al agente (`POST /resync`) en `run -from`, `fork` y `thaw`. Sin él, `now()` estaría parado en el instante del dorado |
| CSPRNG del kernel del invitado | Se corrige solo: el mismo `/resync` mete entropía fresca y fuerza la resiembra (`RNDRESEEDCRNG`), y en Linux además actúa VMGenID |
| IP y red | Igual en todas (`172.16.0.2`); lo que las distingue está en el host (un netns por máquina) |
| Volúmenes | No hay (por diseño, ver arriba) |

Lo que el núcleo **no puede** corregir, porque vive dentro del proceso Postgres:

- **Estado de aleatoriedad del postmaster.** Un proceso que ya tenía su semilla en
  memoria la conserva (lo dice `fork.go`). Los backends nuevos se resiembran al hacer
  `fork` del postmaster y `random()`/`gen_random_uuid()` salen distintos (el script de
  comprobación lo verifica), pero las **claves de cancelación** las genera el postmaster,
  y con OpenSSL su generador podría repetir la secuencia entre copias hasta su próxima
  resiembra. Es un riesgo bajo (cada copia vive en su red privada y una clave de
  cancelación solo permite cancelar una consulta), pero real. `db-post-thaw.sh -restart`
  reinicia el postmaster y lo elimina, a costa de la caché caliente.
- **Marcas de tiempo internas.** `pg_postmaster_start_time()`, `backend_start` de los
  procesos de fondo y `pg_stat_*` conservan la hora del dorado. Es la hora real en que
  arrancó el proceso, no un error.
- **Un salto adelante del reloj.** El checkpointer y el autovacuum miden con reloj de
  pared: al saltar el reloj horas o días adelante, pueden hacer un checkpoint o una
  pasada de autovacuum nada más despertar. Es inocuo, pero explica un pico de E/S al
  arrancar.
- **Conexiones heredadas.** Un dorado con clientes conectados repartiría sus sockets y
  claves a todas las copias: `db-golden.sh` se niega a congelar con alguno.
- **PIDs.** `pg_backend_pid()` se repite entre copias (misma tabla de PIDs en máquinas
  distintas). No es un fallo; un cliente que use el PID como identidad global lo sería.
- **Identidad del cluster.** Todas comparten `system_identifier`, OIDs y `pg_control`.
  Copias de un mismo dorado no pueden ser réplica una de otra ni mezclar WAL.

`scripts/db-post-thaw.sh <copia>` es el paso posterior: espera al postmaster, exige que
el reloj del invitado y `now()` estén a menos de 5 s del host (si no, el `/resync` no se
aplicó: agente viejo o fallo, mira el log del daemon), y avisa de clientes heredados.
`db-golden-verify.sh` lo corre por cada copia.

## Comprobaciones en el lab

Con el daemon y la imagen `pg16` construida:

```sh
scripts/db-golden.sh build -seed-mb 50 verifdb
scripts/db-golden-verify.sh -n 4 verifdb          # 4 copias a la vez; -keep para inspeccionarlas
```

Comprueba, en cada copia y entre ellas: `now()` frente al host; conexión por red como el
rol de la app con SCRAM; `pg_stat_activity` sin clientes heredados; escrituras en
paralelo en todas las copias (cada una ve solo lo suyo); `gen_random_uuid()` y
`random()` distintos; y **informa** de los `pg_backend_pid()` repetidos. Salida
esperada: `db-golden-verify: todo bien`.

A mano, sobre una copia (`kling run -from verifdb -name c1`):

```sh
scripts/db-post-thaw.sh c1
kling exec c1 -- su -s /bin/sh postgres -c "psql -X -d postgres -c 'select now(), pg_postmaster_start_time(), pg_is_in_recovery()'"
date -u                                            # en el host: now() tiene que coincidir
kling exec c1 -- su -s /bin/sh postgres -c "psql -X -d postgres -c 'select pid, backend_type, backend_start from pg_stat_activity order by 1'"
kling exec c1 -- tail -n 20 /var/log/postgresql/pg.log     # ¿mensajes de reloj o de recuperación?
```

También hay que medir, y el script no lo hace: tiempo de `run -from` (esperado: decenas
de ms), tamaño del snapshot (`kling template ls`: la memoria del dorado más
`shared_buffers`), y el consumo de memoria de N copias (`kling top`).

## Límites

- **Disco: 512 MiB por máquina.** El overlay (`defaultOverlayMiB`) lo comparten el
  cluster (~40 MiB tras `initdb`), el WAL (`max_wal_size = 96MB`) y tus datos.
  `-seed-mb` se limita a 250 y el script aborta si el overlay pasa del 85 %. Cada copia
  puede escribir lo que queda. Una base mayor pide subir esa constante (Go).
- **Sin red.** La máquina de preparación no tiene egress: no hay `pg_dump` desde fuera ni
  extensiones descargadas; todo entra por `kling cp` (hasta 64 MiB por fichero) y por la
  imagen. Los migrations deben poder correr sin red.
- **`wal_level = minimal`**: sin réplicas, `pg_basebackup` ni archivado. Es lo que da
  cabida en el overlay; cambiarlo es editar `db-golden.sh`.
- **Una versión, un locale.** PG16, `UTF8` con `C.UTF-8` (ordenación por bytes). Cambiar
  de locale implica otro `initdb`.
- **Un dorado por cluster.** La plantilla no se actualiza: para cambiar migraciones hay
  que reconstruirla.
- **Memoria.** El dorado guarda la RAM del invitado, `shared_buffers` (128 MB) incluidos:
  es lo que hace la primera consulta rápida y lo que ocupa disco. Las copias comparten
  esas páginas hasta que las modifican.
- **Cifrado en reposo.** Los snapshots guardan la memoria en claro (ver
  [cifrado.md](cifrado.md)). En la memoria del dorado están los datos y el hash SCRAM del
  rol, no su contraseña; el fichero `password` del host es lo único sensible.
- **`exec` es de confianza.** Quien tiene `kling exec` en la máquina es root en ella y
  puede leer el cluster. Esa frontera es la del host, no la del invitado.
- **La contraseña por stdin** pasa por el socket del daemon, que ya es de root. Lo que se
  evita es argv, historial de shell, pantalla y log de Postgres.
- **Lo que no se ha ejecutado.** Los scripts se han comprobado con `bash -n`,
  `shellcheck` y un `kling` simulado; lo que ocurre dentro de la microVM (paquetes de
  Alpine, `initdb`, el arranque con `su`) se comprueba por primera vez en el lab.

## Siguientes nodos

Conectar la copia al invitado sin que vea la contraseña es el proxy de
[postgres.md](postgres.md): el fichero `password` es el que `kling machine credential -type
postgres -upstream <ip-de-la-copia>:5432 -upstream-tls disable -f password` espera, con
SCRAM sin TLS (que es lo que el servidor de esta imagen pide). La IP de cada copia es la
de su netns (`kling inspect <copia>`, campo `ip`), no `172.16.0.2`.

## En macOS: el golden desde una plantilla

El backend vz no construye imágenes (hace falta root, dispositivos loop y chroot en Linux).
En su lugar, `db-golden.sh build -from <plantilla>` arranca desde una plantilla que ya trae
Postgres instalado, con la memoria y las CPU de su snapshot y el egress forzado a `none`:

```sh
kling run -image toolchain -egress internet -allow-exec -mem 1G -cpus 2 -name pgbase
kling exec pgbase -- apk add postgresql16 postgresql16-client postgresql16-contrib tzdata
kling save pgbase pg16base
scripts/db-golden.sh build -from pg16base -seed-mb 20 pg
```

Medido en un Mac M4 (APFS, clonefile) el 2026-09-29, con un golden de 20 MB de seed:
`kling db up` deja una copia lista y con la clave rotada en 526–562 ms (la primera, en frío,
1,0 s); `run -from` solo, 318–357 ms. En el Mac el coste está en la restauración de vz, no
en la copia del disco, así que clonefile no lo hace más rápido que el lab Linux (ext4).
`kling db doctor -url` contra una copia marca como crítico al superusuario `postgres`
porque desde una URL no sabe que en una copia solo entra por el socket local; para una
copia se usa `kling db doctor <copia>`, que lo informa como INFO.
