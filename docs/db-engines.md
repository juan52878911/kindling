# kling db con Redis y SQLite (y por qué no MongoDB)

`kling db` nació para Postgres ([db.md](db.md)) y ya habla MySQL/MariaDB
([mysql.md](mysql.md)). Esta página cubre los otros dos motores (#65): **Redis**, con el
mismo modelo que MySQL (servidor caliente en la microVM, clave propia por copia que solo
conoce el host), y **SQLite**, que no tiene servidor y por eso tiene un modelo propio y
más sencillo. El modelo de seguridad está en [SECURITY.md §18](../SECURITY.md).

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
| `attach`/`detach`, `role`, `rehearse`, `snapshot`/`undo`, `tenant-check`, `ask`, `ask-web`, `diff`, `env`, `clone` | sí | no | no | no |

Lo que un motor no hace se rechaza **antes de tocar nada**, con
`kling db <cmd> supports postgres copies only in this version; <copia> is redis (see
docs/db-engines.md)` (o la lista de motores que sí). La etiqueta que lo decide es
`kling.db.engine` (`redis`, `sqlite`), que ponen los scripts de plantilla y heredan las
copias.

## Redis

### La plantilla (`scripts/db-golden-redis.sh`)

- **Receta** `scripts/recipes/redis.recipe.json`: Alpine + `redis` y `tzdata`. El script
  vale igual con `valkey-server`/`valkey-cli` (busca los dos nombres), por si la versión
  de Alpine solo empaqueta Valkey. Hace falta Redis 7 o más (ACL con selectores,
  `ACL DRYRUN`, `enable-*-command`).
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

Si MongoDB hace falta, la receta es: una imagen glibc con `mongod` (fuera de kindling),
una plantilla hecha a mano con `-from`, y la clave rotada con `updateUser` aceptando que
cruza al invitado por stdin (nunca en argv), documentando esa diferencia. No se incluye
aquí.

## Probado y pendiente

Probado con fakes (`ext/db/cmd/kling-db/engines_test.go`,
`ext/db/internal/doctor/engines_test.go`): las copias nacen con su motor y su puerto,
reciben solo el hash, cada copia estrena clave (y la del administrador en Redis), una
rotación que falla destruye la copia o deja la vieja, `connect` en cada modo, y todos los
rechazos sin tocar la máquina. Los scripts pasan `bash -n` y `shellcheck`.

Pendiente de ejecutar en el lab (sin hacer en este cambio, porque exige construir las
imágenes y eso descarga paquetes): `db-golden-redis.sh`/`db-golden-sqlite.sh image` y
`build`, y la sección 7h2 de `scripts/90-e2e.sh`
(`KLING_E2E_REDIS_GOLDEN=rd KLING_E2E_SQLITE_GOLDEN=sq`). Lo que más puede sorprender en
el primer uso real: el formato exacto de la salida de `redis-cli` fuera de una terminal
(el guion compara `OK`, `PONG` y lee `ACL GETUSER` línea a línea) y la versión de Redis
que traiga la Alpine del builder.
