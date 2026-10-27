# kling db con Redis, SQLite y FerretDB (y por qué no MongoDB)

`kling db` nació para Postgres ([db.md](db.md)) y ya habla MySQL/MariaDB
([mysql.md](mysql.md)). Esta página cubre los otros dos motores (#65): **Redis**, con el
mismo modelo que MySQL (servidor caliente en la microVM, clave propia por copia que solo
conoce el host), y **SQLite**, que no tiene servidor y por eso tiene un modelo propio y
más sencillo. El modelo de seguridad está en [SECURITY.md §19](../SECURITY.md).

```sh
# Redis
kling db golden -script scripts/db-golden.sh image -engine redis           # la imagen, una vez
kling db golden -script scripts/db-golden.sh build -engine redis -seed-keys 100000 rd
kling db up rd -name r1                 # una copia lista, con su propia contraseña
kling db connect r1 -redis              # el redis-cli del host (clave en REDISCLI_AUTH)
kling db connect r1 -dsn                # redis://app:…@IP:6379/0
kling db fork r1 -n 4 · kling db rotate r1 · kling db doctor r1 · kling db rm r1

# SQLite
kling db golden -script scripts/db-golden.sh image -engine sqlite
kling db golden -script scripts/db-golden.sh build -engine sqlite -migrations db/ -seed seed.sql sq
kling db up sq -name s1                 # una microVM con /var/lib/kling-db/appdb.sqlite
kling db connect s1 -sqlite             # sqlite3 dentro de la copia (kling shell)
kling exec -i s1 -- sqlite3 -bail /var/lib/kling-db/appdb.sqlite < consulta.sql
kling db fork s1 -n 4 · kling db reset s1 · kling db doctor s1 · kling db rm s1
```

## Qué funciona con cada motor

| comando | Postgres | MySQL | Redis | SQLite |
|---|---|---|---|---|
| `up`, `fork`, `reset`, `rm` | sí | sí | sí | sí |
| `connect` (info y `-dsn`) | `-psql` | `-mysql` | `-redis`, `redis://` | `-sqlite`; sin DSN |
| `rotate` | sí | sí | sí (solo el usuario de la app) | no: no hay clave |
| `doctor <copia>` | DB | MY | RD (básico) | SQ (básico) |
| `audit` | sí | sí | no | no |
| `branch` | sí | sí | no | no |
| `class` | sí | sí | sí | sí (sin clave: se entra con `-sqlite`) |
| `attach`/`detach`, `role`, `rehearse`, `snapshot`/`undo`, `tenant-check`, `ask`, `ask-web`, `report`, `diff`, `env`, `clone`, `slice`, `observe` | sí | no | no | no |

Lo que un motor no hace se rechaza **antes de tocar nada**, con
`kling db <cmd> supports postgres copies only in this version; <copia> is redis (see
docs/db-engines.md)` (o la lista de motores que sí). La etiqueta que lo decide es
`kling.db.engine` (`redis`, `sqlite`), que ponen los scripts de plantilla y heredan las
copias.

## Redis

### La plantilla (`scripts/db-golden-redis.sh`)

- **Receta** `scripts/recipes/redis.recipe.json`: Alpine + `redis` y `tzdata`. Hace falta
  Redis 7 o más (ACL con selectores, `ACL DRYRUN`, `enable-*-command`).
- **Valkey** (el fork libre de Redis): `kling db golden image -engine valkey` construye la
  imagen `valkey` con `scripts/recipes/valkey.recipe.json` (Alpine + `valkey` y
  `valkey-cli`, 9.0.4 en Alpine 3.24) y `golden build -engine valkey <nombre>` la usa por
  defecto. El script busca `valkey-server`/`valkey-cli` y el usuario `valkey` si no hay
  `redis`; lo demás es igual, y `kling db` la trata como `redis` (mismo protocolo, mismas
  ACL). Probado en el lab el 2026-10-11: dorado con 1.000 claves, copia con `kling db up`,
  `AUTH` del usuario de la aplicación, `CONFIG` denegado y `kling db doctor` sin hallazgos.
- **El servidor** escucha en la IP del invitado y en `/run/redis/redis.sock`
  (`unixsocketperm 700`, del usuario `redis`), como el usuario `redis`, con la
  configuración en `/etc/kling-db/redis.conf`: `protected-mode yes`, sin `DEBUG` ni
  `MODULE` (`enable-debug-command no`, `enable-module-command no`,
  `enable-protected-configs no`), `maxclients 100`, sin instantáneas automáticas ni AOF.
  Los datos viven en la RAM de la microVM congelada; el `dump.rdb` que deja el script
  (un `SAVE` al final) solo sirve si el servidor se reinicia dentro de una copia.
- **Dos usuarios ACL** (`/var/lib/redis/users.acl`, que reescribe `ACL SAVE`):
  - `default`, el **administrador** (`+@all`). Su clave se genera **dentro** del invitado
    y vive en `/etc/kling-db/redis-admin` (0600, root): no sale nunca de la máquina. Es
    como entra `kling db` (y quien sea root dentro), con `REDISCLI_AUTH` en el entorno.
  - `app` (`-role`), el de la **aplicación**: `~* &* +@all -@admin` (ni `CONFIG`, ni
    `ACL`, ni `SHUTDOWN`, ni `MONITOR`, ni `MODULE`). Su clave la genera el **host**
    (192 bits) y se queda en `$STATE/<nombre>/password` (0600); al invitado va su
    **SHA-256**, que es lo que Redis guarda (`#<hash>`), por stdin. No hay camino en claro.
- **Seed**: `-seed FILE` son comandos de Redis, uno por línea, que ejecuta `redis-cli`
  como administrador; los errores se cuentan y paran la construcción. `-seed-keys N`
  crea N claves sintéticas de ~100 bytes. No hay `-migrations` (no hay SQL).
- **Antes de congelar**: ningún usuario `nopass`, exactamente dos usuarios, `app` con su
  hash y nada más, ningún cliente conectado (se repartiría a todas las copias).

### La rotación en cada copia

`kling db up`, `fork` y `reset` mandan al invitado, por stdin de `kling exec -i ... sh -s`,
un guion que:

1. **estrena la clave del administrador** dentro del invitado (192 bits de
   `/dev/urandom`, su SHA-256 a `ACL SETUSER default resetpass #<hash>`); el fichero nuevo
   se escribe antes y solo sustituye al viejo cuando la clave nueva ya contesta a `PING`:
   nunca queda un administrador sin clave conocida. Así dos copias del mismo dorado no
   comparten ninguna clave, tampoco la de administración;
2. pone al usuario de la aplicación el SHA-256 de su clave nueva (generada en el host y
   escrita en `copies/<id>/password` **antes**), `ACL SAVE`, y enseña `ACL GETUSER app`;
3. kling db comprueba en esa salida que el usuario está activo y tiene **exactamente**
   ese hash. Si no, la copia se destruye; hasta entonces está en `preparing` y `connect`
   la rechaza.

`kling db rotate` hace solo el paso 2 (con la clave vieja de vuelta si falla, como en
Postgres). La clave nunca va en argv, ni la del host ni la del administrador.

### connect

Sin flags, dirección, puerto (6379), usuario y la ruta del fichero de la clave. `-dsn`
da `redis://app:<clave>@<host>:6379/0` (pregunta si stdout es una terminal). `-redis`
abre el `redis-cli` (o `valkey-cli`) del host con `--user app` y la clave en
`REDISCLI_AUTH`. Sin TLS: el tramo es el del host a su propio invitado. En macOS va por el
reenvío que abre `kling.ports=6379`.

### doctor (reglas RD)

| regla | qué |
|---|---|
| RD001 | CRITICAL: un usuario activo con `nopass` |
| RD002 | HIGH: el usuario de la aplicación puede ejecutar comandos de administración (`ACL DRYRUN app CONFIG GET maxmemory` da `OK`) |
| RD003 | WARN: usuarios activos además de `default` y el de la aplicación |
| RD051 | INFO: conexiones de cliente abiertas (sin contar la del doctor) |
| RD052 | la clave del usuario sigue siendo la del dorado (CRITICAL), no tiene exactamente una, o el usuario no existe |
| RD053 | `kling.db.state` no es `ready` |
| RD054 | el fichero de la clave en el host falta, está abierto a otros o no casa con el hash |

`doctor -url redis://...` no existe.

## SQLite

### El modelo

SQLite no tiene servidor, red ni usuarios: la base es un fichero. La copia es una
microVM con `/var/lib/kling-db/<base>.sqlite` (0600, root, directorio 0700) y el cliente
`sqlite3`. Se entra con `kling exec` o `kling shell`, que ya exigen poder operar la
máquina en el daemon (su dueño, con [autorización](authz.md)); no hay más frontera que
esa, y no se inventa una:

- **no hay contraseña**: ni en el host ni en el invitado. `checkReady` no pide fichero de
  clave para una copia SQLite (no hay nada que una copia heredada pudiera reutilizar);
  sí exige que sea propia, esté corriendo y en `ready`;
- `connect` enseña el fichero y cómo entrar; `connect -sqlite` abre `sqlite3` dentro con
  `kling shell`; `-dsn` se rechaza (no hay nada a lo que conectar desde fuera);
- `rotate` se rechaza: no hay clave.

`up`, `fork` y `reset` solo comprueban, antes de marcar `ready`, que la base existe y
`sqlite3 -readonly` la abre (`PRAGMA schema_version`); `sqlite3` crearía una vacía si no
existiera, por eso se mira primero que el fichero esté.

### La plantilla (`scripts/db-golden-sqlite.sh`)

- **Receta** `scripts/recipes/sqlite.recipe.json`: Alpine + `sqlite` y `tzdata`.
- `-migrations DIR` y `-seed FILE` (SQL, con `sqlite3 -bail`), o `-seed-mb N`
  sintéticos (CTE recursiva). `-database B` cambia el nombre del fichero. No hay
  `-role`.
- Diario de siempre (`DELETE`, no WAL): no quedan `-wal`/`-shm` a medias al congelar ni
  estorban a una lectura `-readonly`. Antes de congelar, `ANALYZE`, `PRAGMA quick_check`
  (tiene que dar `ok`) y `sync`.
- `conn.env` dice `ENGINE=sqlite` y `DBNAME`; no hay fichero de contraseña.

### doctor (reglas SQ)

| regla | qué |
|---|---|
| SQ001 | HIGH: `PRAGMA quick_check` no da `ok` (se abre en solo lectura) |
| SQ002 | WARN: el fichero lo puede leer o escribir cualquier usuario del invitado |
| SQ050 | INFO: sin servidor ni clave; se entra con `kling exec`/`kling shell` |
| SQ053 | `kling.db.state` no es `ready` |

## Sin proxy de credenciales

El proxy de credenciales de kindling habla Postgres y MySQL. Para Redis y SQLite **no
hay** proxy en esta versión, y por eso `attach`/`detach` se rechazan:

- **Redis**: el `AUTH` de RESP manda la clave en claro por el socket, así que un proxy
  tendría que cambiar el marcador por la clave (como el de Postgres), pero RESP no tiene
  un saludo del servidor ni una prueba de que conoce la clave. Es trabajo propio, con su
  revisión de seguridad; hasta entonces un agente de **otra** microVM no recibe una copia
  Redis. Un agente en la misma microVM usa la copia como siempre.
- **SQLite**: no hay protocolo que proxificar. Compartir la base es compartir la máquina.

## ClickHouse

`kling db golden image -engine clickhouse` importa la imagen oficial de Docker
(`clickhouse/clickhouse-server:25.8`, glibc) sin Docker, y `golden build -engine clickhouse
<nombre>` deja una plantilla con el servidor caliente:

```sh
scripts/db-golden.sh image -engine clickhouse
scripts/db-golden.sh build -engine clickhouse -seed-rows 100000 eventos
kling db up eventos -name e1
kling db connect e1 -dsn   # clickhouse://app:…@<ip>:9000/appdb  (HTTP en el 8123)
```

- **La clave cumple la regla del hash.** El usuario de la aplicación se crea y se rota
  con `IDENTIFIED WITH sha256_hash BY '<hex>'`, y el hash se calcula en el host. La
  rotación comprueba con `SHOW CREATE USER` que se guardó: la plantilla enciende
  `display_secrets_in_show_and_select` para eso.
- **El administrador** es `default`, sin clave y solo desde el loopback del invitado
  (`HOST LOCAL`). Por red solo entra el usuario de la aplicación, con `GRANT ALL` en su base.
- **El servidor** escucha en la IP del invitado y en 127.0.0.1 (la sonda de "listo" de la
  imagen pregunta ahí), y no en todas las direcciones como trae la imagen.
- **`kling db doctor`** mira que `default` sea solo local (CH001), que no haya más usuarios
  (CH003) y que la clave de la aplicación no sea la del dorado y cuadre con el fichero del
  host (CH052, CH054).
- Probado en el lab el 2026-10-11 con 100.000 filas:
  - la copia entra con su clave por HTTP y la del dorado se rechaza;
  - `rotate` cambia la clave y la vieja deja de valer;
  - `default` no entra por red;
  - `doctor` no encuentra nada.
- No van con ClickHouse: `attach`, `role`, `branch`, `snapshot`/`undo`, `ask` y `clone`.

## Almacén de objetos S3: Garage

`kling db golden image -engine garage` importa la imagen oficial `dxflrs/garage` (AGPL,
pero se usa como servidor, sin tocarla; no MinIO) sin Docker, y `golden build -engine
garage <nombre>` deja una plantilla con el servidor caliente y un bucket de semilla:

```sh
scripts/db-golden.sh image -engine garage
scripts/db-golden.sh build -engine garage -seed-objects 50 almacen   # bucket appdb, 50 objetos de 4 KiB
kling db up almacen -name s1
kling db connect s1 -dsn   # s3://GK…:<secreto>@<ip>:3900/appdb   (path-style, sin TLS, región garage)
```

- **La clave es de la copia.** SigV4 es un HMAC: Garage necesita el secreto, no un hash. Cada
  copia estrena su propio par acceso/secreto aleatorio, calculado en el host (el secreto es el
  fichero de contraseña de la copia; el identificador, `GK` + 24 hexadecimales, sale de él con
  SHA-256), importa ese par (`garage key import`), le da lectura, escritura y dueño sobre el
  bucket y **borra el par del dorado y cualquier otro**. `rotate` hace lo mismo. Límite: el
  secreto sí entra en el invitado (argv de `garage key import`, visible solo dentro de la
  copia), porque no hay forma de dar a Garage un verificador; y la clave rotada solo da acceso
  al bucket de la plantilla, no a otros que se creen después.
- **La imagen no tiene shell** (`scratch`): el servicio es `/garage -c /etc/garage.toml server`,
  que reintenta hasta que `build` copia la configuración con `kling cp`; toda orden de
  administración va por argv directo. El RPC escucha solo en 127.0.0.1 y el administrador no
  es accesible por red (solo el S3 en el 3900).
- **Cliente:** cualquiera con SigV4 (`aws`, `rclone`, SDK) con endpoint `http://<ip>:3900`,
  región `garage` y path-style. La carga de la semilla usa `curl --aws-sigv4` desde el host
  (Linux: llega a la IP del invitado); Garage exige la cabecera `x-amz-content-sha256`.
- **`doctor`** mira que haya exactamente una clave (GA052), que no sea la del dorado (GA052,
  crítico) y que cuadre con el secreto del host (GA054).
- Probado en el lab (kling-ascci, daemon privado, 2026-10-27, Garage 1.1.0, 50 objetos):
  - dos copias listan los 50 objetos; un PUT en una no aparece en la otra;
  - la clave de una copia da 403 en la otra, y la del dorado da 403 en las dos;
  - `rotate` cambia el par y el viejo da 403; `reset` devuelve el bucket al estado del
    dorado (el PUT desaparece); `fork` de una copia da copias con sus datos y claves nuevas;
  - `doctor` no encuentra nada;
  - `up` desde la plantilla: 1,26, 1,12 y 1,29 s; `reset`: 1,64, 1,84 y 1,66 s (Redis, mismo
    host: 0,51 / 0,42 / 0,51 s y 0,61 / 0,58 / 0,54 s). Cada paso de administración es un
    `exec` a un binario Rust que abre su RPC, de ahí la diferencia.
- No van con Garage: `attach`, `role`, `branch`, `snapshot`/`undo`, `ask` y `clone`. Volver
  al estado del dorado es `reset`.

## Por qué no MongoDB

Se estudió y se deja fuera a propósito:

1. **No hay paquete que usar.** Alpine sacó `mongodb` de sus repositorios (la licencia
   SSPL no es libre para sus criterios) y la receta de imágenes de kindling se construye
   con paquetes de Alpine. Los binarios oficiales de MongoDB están compilados contra
   glibc: habría que descargarlos (fuera de este cambio: nada se descarga) y montar una
   base glibc (`scripts/71-build-glibc-base.sh`) solo para él.
2. **La clave entraría en el invitado.** El modelo de `kling db` es que al invitado solo
   va un hash o un verificador, nunca la clave. En MongoDB, `createUser` y `updateUser`
   reciben la contraseña **en claro** (`pwd`) y el servidor calcula su SCRAM; la única
   forma de dar solo el verificador es escribir a mano en `admin.system.users`, que no es
   una interfaz soportada. Hacerlo así sería frágil, y hacerlo con la clave en claro
   rompería la regla que sostienen los otros cuatro motores.

Si hace falta el **protocolo** de MongoDB, la vía es FerretDB (abajo). MongoDB en sí,
con `mongod`, sigue fuera.

## Compatible con MongoDB: FerretDB

[FerretDB](https://www.ferretdb.com) (Apache-2.0) habla el protocolo de MongoDB sobre un
Postgres con la extensión DocumentDB. Una copia es por dentro un Postgres, y la regla del
verificador se cumple igual. Comprobado en el lab: FerretDB 2 valida el login de Mongo
(SCRAM-SHA-256) contra la contraseña del **rol de Postgres**. Así que `kling db up` y
`rotate` le ponen un verificador SCRAM calculado en el host, como a cualquier copia de
Postgres, y el cliente de Mongo entra con ese usuario y esa clave.

```sh
scripts/db-golden.sh image -engine ferretdb              # kling image import ghcr.io/ferretdb/ferretdb-eval:2 → ferretdb
scripts/db-golden.sh build -engine ferretdb -seed-docs 1000 docs
kling db up docs -name d1
kling db connect d1 -dsn   # mongodb://app:…@<ip>:27017/appdb?authMechanism=SCRAM-SHA-256
```

- **La imagen** es la oficial de evaluación (Postgres 17 + DocumentDB + FerretDB 2.7),
  importada sin Docker. Alpine no empaqueta DocumentDB.
- **El superusuario** `postgres` lleva una clave generada dentro del invitado, que solo
  usa FerretDB por el loopback. Por red, `pg_hba.conf` solo deja entrar al rol de la
  aplicación, con SCRAM.
- **El rol de la aplicación** lleva `documentdb_admin_role`: FerretDB 2 aún no tiene
  permisos por base, así que ve todas las bases de Mongo de su copia (todas son suyas).
- **`-seed-docs N`** carga N documentos sintéticos por SQL
  (`documentdb_api.insert_one`): no hace falta cliente de Mongo en el host. Para datos
  reales, cárgalos con tu driver en una copia y guárdala como plantilla.
- **`kling db connect`** da el DSN (`-dsn`); no abre `mongosh` porque pediría la clave en
  argv o por teclado. `doctor` no cuenta como cliente heredado el pool de FerretDB (el
  superusuario por el loopback).
- Probado en el lab el 2026-10-11 con un cliente del driver oficial de Go:
  - un dorado con 1.000 documentos, la copia con `kling db up` y el login con la clave
    rotada;
  - la clave del dorado se rechaza;
  - `kling db rotate` cambia la clave y la vieja ya no entra;
  - `kling db doctor` sin avisos.
- No van con FerretDB: `attach`, `role`, `branch`, `snapshot`/`undo`, `ask` y `clone`, que
  dependen de las tablas de la aplicación en SQL; se rechazan con un error claro.

## Probado y pendiente

Probado con fakes (`ext/db/cmd/kling-db/engines_test.go`,
`ext/db/internal/doctor/engines_test.go`): las copias nacen con su motor y su puerto,
reciben solo el hash, cada copia estrena clave (y la del administrador en Redis), una
rotación que falla destruye la copia o deja la vieja, `connect` en cada modo, y todos los
rechazos sin tocar la máquina. Los scripts pasan `bash -n` y `shellcheck`.

Probado en el lab (CT 105, 2026-09-29): las imágenes y los goldens de Redis y SQLite se
construyeron de verdad sobre Alpine, y la sección 7h2 de `scripts/90-e2e.sh`
(`KLING_E2E_REDIS_GOLDEN=rd KLING_E2E_SQLITE_GOLDEN=sq`) pasa entera. Eso incluye up, fork
con clave propia, rotate con el hash nuevo en la copia, doctor con 0 problemas, los
rechazos y ninguna clave en la salida. `kling db class` también se probó con los dos
motores.

## Próximas funcionalidades

Análisis de compatibilidad del 2026-09-29. Hay tres niveles de soporte:

1. **Golden y copia al instante.** Vale para cualquier motor que corra en Linux, porque
   una microVM congelada no depende de lo que haya dentro.
2. **Clave por copia sin que el invitado vea la clave.** Al invitado solo le llega un hash
   o un verificador; hace falta que el motor acepte un hash calculado en el host.
3. **Proxy de credenciales o `attach`.** El servidor tiene que demostrar que conoce la
   clave; hoy solo Postgres (SCRAM) lo hace bien.

Por orden:

1. **Hecho.** pgvector y PostGIS (`kling db golden image -ext`, #144) y un golden de
   Valkey (`-engine valkey`, #113).
2. **Hecho.** MySQL 8.4 y 9 de Oracle, con el hash de `caching_sha2_password` calculado en
   el host (#114, docs/mysql.md). Percona y los orígenes de `clone` en MySQL siguen
   pendientes.
3. **Almacenamiento de objetos (S3).**
   - **Hecho** (#115, Garage, arriba): golden con clave por copia. SigV4 es HMAC, así que el
     servidor necesita el secreto: el invitado conoce el de su copia (aleatorio y suyo),
     nunca el del dorado.
   - Un modo de **re-firma SigV4** en el proxy HTTP. El invitado firma con una credencial
     marcador y el proxy descarta esa firma y firma de nuevo con la real. Lo delicado: el
     hash del cuerpo (`UNSIGNED-PAYLOAD`), las subidas `aws-chunked`, con una firma por
     trozo, y las URL prefirmadas. Con él, un agente usa S3, R2, GCS en modo S3, MinIO o
     Garage sin ver la clave.
   - Opcional: `clone` de un bucket por prefijo hacia el golden.
4. **Hecho.** Compatible con MongoDB mediante FerretDB (#117, arriba).
5. **Hecho.** ClickHouse desde su imagen de Docker (#118, arriba).

Descartados por ahora: Cassandra y Scylla (pesados), y Elasticsearch y OpenSearch (JVM
con 1 GiB o más por copia; posibles con un hash bcrypt calculado en el host si hay
demanda). KeyDB y Dragonfly no son compatibles del todo con las ACL de Redis 7.
