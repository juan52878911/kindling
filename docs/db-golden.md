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
| Construcción completa | **180 s** (TimescaleDB ~80 s; PostGIS ~50 s; pgvector, pg_cron y pg_partman, segundos). Con la máquina de construcción a un solo núcleo (`-cpu-pct 100` con 4 vCPU), 466 s; `pg17-ext` se midió así: 477 s |
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
Corren como el rol de la aplicación (así los objetos son suyos). Para las extensiones
que solo crea un superusuario (TimescaleDB, PostGIS, pgvector...) está `-extension`:

```sh
kling db golden build -from pg16-ext \
  -extension timescaledb,vector,uuid-ossp,pgcrypto -preload timescaledb \
  -conf work_mem=16MB -migrations infrastructure/postgres/init -role crm_user -database crm_db aura
```

- `-extension A,B` hace `CREATE EXTENSION IF NOT EXISTS ... CASCADE` **como superusuario
  y antes de las migraciones**, que siguen corriendo como el rol de la aplicación: el
  `CREATE EXTENSION IF NOT EXISTS` de las migraciones pasa a no hacer nada, los objetos
  siguen siendo del rol y el rol **no** es superusuario.
- `-preload A,B` va a `shared_preload_libraries`, sumado a lo que ya traiga la plantilla
  (`pg16-ext` precarga `timescaledb`). Cada librería se comprueba antes de arrancar.
- `-conf CLAVE=VALOR` (repetible) añade una línea a `postgresql.conf`. Las que sostienen
  la seguridad y la auditoría del golden (`listen_addresses`, `password_encryption`,
  `ssl*`, `log_*`, ficheros de configuración...) se rechazan.
- **Antes de ejecutar nada** se comprueban todas las extensiones: las de `-extension` y
  las que crean las migraciones (`CREATE EXTENSION` fuera de comentarios). Si faltan, el
  error las lista **todas**, con el fichero y la línea que las pide, y dice cómo tener
  una plantilla que las traiga. Si una migración crea una que solo puede crear un
  superusuario y no está en `-extension`, también se dice antes de empezar.
- Si una migración falla, el error de psql sale con **el fichero y la línea de verdad**
  (no la copia numerada dentro de la microVM).

`-as-super` sigue existiendo (todo como superusuario, objetos del superusuario), para
migraciones que de verdad lo necesitan.

### Un directorio de init al estilo Docker: `-init`

`-init DIR` ejecuta un directorio como `docker-entrypoint-initdb.d`: `*.sql`, `*.sql.gz` y
`*.sh` en orden, desde el propio directorio (un `\i` a un `.psql` hermano funciona), con
`POSTGRES_USER`, `POSTGRES_DB`, `PGHOST`, `PGUSER` y `PGDATABASE` en el entorno, más lo
de `-env-file F` (líneas `CLAVE=VALOR`, 0600: las claves no van por argv; cada valor se
toma literal, como `docker --env-file`, sin ejecutar nada del fichero). Los `.sh` con
`#!/bin/bash` necesitan bash: `pg16-ext` lo trae. Va después de `-extension` y antes de
`-migrations`.

Diferencia a propósito con Docker: allí corre como el superusuario (que es
`POSTGRES_USER`). Aquí corre **como el rol de la aplicación**, que no es superusuario: los
objetos son suyos y la RLS le aplica. Para lo que un init suele hacer además (crear roles:
`app_user`, uno por servicio), el rol tiene `CREATEROLE` **solo mientras dura `-init`** y
el socket local le deja entrar sin clave **solo mientras dura** (la máquina de preparación
no tiene red); después se le quita y se borra la línea de `pg_hba.conf`.

### Migraciones que hace un programa: `-step`, `-super-step`, `-sql`

Cuando el esquema lo hace un programa (alembic, prisma, flyway, `manage.py migrate`) y no
un `.sql`, `golden build` encadena lo que antes se hacía a mano:

```sh
kling db golden build -from pg16-ext -extension timescaledb,vector,uuid-ossp,pgcrypto \
  -role crm_user -database crm_db -init infrastructure/postgres/init -env-file ~/aura.env \
  -agent aura-py -workdir . \
  -step 'for s in backend/*/; do (cd $s && alembic upgrade head) || exit 1; done' \
  -super-step 'bash infrastructure/rls/aplicar_rls.sh --confirmar' \
  -sql infrastructure/demo-seed.sql \
  aura-main
```

1. `db-golden.sh` construye `<nombre>-base` con todo lo de siempre (extensiones, `-init`,
   `-migrations`...).
2. Una copia de ese golden (`kling db up`), con su clave rotada.
3. Si hay `-step`: una microVM de `-agent` (una plantilla con el programa y sus
   dependencias; egress `allowlist`, así que solo llega a la copia) a la que se sube
   `-workdir` en `/work` (sin `.git`, `node_modules`, `__pycache__` ni enlaces; hasta 64
   MiB comprimido) y `-env-file`, y que recibe la copia por `kling db attach`. Cada
   `-step` corre con `sh -c` en `/work` con `PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`,
   `PGSSLMODE=disable`, `PGPASSWORD` (un **marcador**: la clave no entra en el agente; el
   proxy la pone) y `DATABASE_URL`. Su salida se ve al momento. Plazo: `-step-timeout`
   (30 min).
4. `-super-step "<cmd>"` corre **dentro de la copia**, como el superusuario por el
   socket local (`-workdir` en `/tmp/kdb-work`): lo que en Docker hacía un script como
   `POSTGRES_USER` superusuario y el rol de la aplicación no puede hacer
   (`ALTER ROLE ... NOSUPERUSER`, por ejemplo), sin hacerlo superusuario a él.
5. `-sql FILE` corre SQL en la copia como el rol de la aplicación, con el error en el
   fichero y la línea de verdad.
6. Pasos, `-super-step` y `-sql`, **en el orden en que se dan**. Si uno falla no se
   guarda nada (y sin `-keep` se borra todo).
7. `detach`, fuera el agente, `VACUUM (ANALYZE)`, `CHECKPOINT`, ningún cliente vivo, el
   log de Postgres vacío, y la copia se guarda como la plantilla `<nombre>` **con su
   contraseña y su `conn.env`**, como cualquier golden (antes, guardar a mano con `kling
   save` dejaba un golden sin ellos, y `doctor` y `diff` no sabían qué rol mirar). La
   copia y `<nombre>-base` se borran.

Para AuraCRM (`main`: 96 tablas, 69 políticas, 4 hypertables, 10 servicios con alembic,
seeds, reparto en esquemas, RLS y demo-seed) el golden sale en **un comando y 28 s** en
el laboratorio; antes eran ~200 s y una docena de pasos a mano. El alembic de los 10
servicios, 11,6 s. Ver la nota de diagnóstico enlazada en `docs/db.md`.

**Por qué el alembic de la nota 18 tardaba 32 s (y Docker 8,5 s).** Medido en el
laboratorio con el de `main` de AuraCRM sobre una copia de `aura-dev` (`rehearse -step`):

| Agente | alembic de los 10 servicios |
|---|---|
| 2 vCPU, `cpu_pct` 50 (el techo por defecto en Linux: media vCPU) | 18,6 s |
| 2 vCPU, `cpu_pct` 100 | 9,6 s |

- Es la **CPU**: importar sqlalchemy, alembic, asyncpg, psycopg2, fastapi y pydantic
  cuesta 0,7–1,2 s con `cpu_pct` 100 y 1,5–2,2 s con 50, y cada servicio arranca un
  Python nuevo (dos, si el primer driver falla). La plantilla del agente de la nota 18 se
  hizo con el techo por defecto.
- **No es la red**: por el proxy de credenciales, una conexión con `select 1` cuesta
  15,6 ms y 200 consultas en una sesión, 107 ms (0,54 ms cada una frente a 0,35 ms
  directas desde el host). Las 390 sentencias del alembic suman ~75 ms de proxy.
- Por eso el agente de `-step` arranca con **toda su CPU** (`cpu_pct` = 100 × vCPUs):
  con una plantilla de techo 50, 9,6 s en vez de 18,6 s. Lo que queda frente a Docker
  (8,5 s) es el arranque en frío de cada Python en una microVM.

`kling exec` y `kling shell` llevan además en su entorno los **marcadores** de las
credenciales de la máquina (los de `kling db attach`, por ejemplo `PGPASSWORD`): antes solo
los veía el servidor de la imagen (por MMDS) y un comando lanzado con `exec` tenía que
leerlos de MMDS a mano. Un marcador no es la clave: solo vale a través del proxy de esa
máquina.

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
