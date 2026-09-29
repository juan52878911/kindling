# kling db: una base Postgres desechable por microVM

`kling db` es la extensión `kling-db` (`ext/db/cmd/kling-db`). Da a un agente o a un
test su propia base Postgres 16, con datos, en milisegundos: una **copia** es una
microVM instanciada de una plantilla con Postgres ya caliente
([db-golden.md](db-golden.md)). La base y el agente viven en la **misma** microVM.

```sh
kling db golden -script scripts/db-golden.sh build -seed-mb 20 pg   # la plantilla, una vez
kling db up pg -name t1                  # una copia lista, con su propia contraseña
kling exec t1 -- su -s /bin/sh postgres -c 'psql -h /run/postgresql appdb'
kling db connect t1 -psql                # desde el host
kling db fork t1 -n 4                    # 4 copias de t1 tal como está ahora
kling db reset t1                        # t1 vuelve a salir de la plantilla
kling db rm t1
kling db doctor t1        ·   kling db audit t1 -since 1h
```

## Subcomandos

| comando | qué hace |
|---|---|
| `up <plantilla> [-name N] [-ttl D] [-owner T]` | `run -from` con `kling.db.state=preparing`, espera a Postgres, **rota la contraseña** y marca `ready` |
| `fork <copia> [-n N]` | descongela si hace falta, `sandbox fork`, pasa cada copia a `preparing`, rota la clave de cada una y las marca `ready`. Todo o nada |
| `connect <copia> [-dsn \| -psql]` | sin flags: dirección, usuario, base y la ruta del fichero de la clave. `-dsn`: el DSN con la clave (pregunta si stdout es una terminal). `-psql`: abre el psql del host con la clave en `PGPASSWORD` |
| `reset <copia>` | `rm` + `up` de la misma plantilla, con el mismo nombre, dueño y ttl |
| `rm <copia>...` | borra la máquina y, después, su contraseña |
| `doctor <copia> \| -url postgres://...` | diagnóstico (`ext/db/internal/doctor`) |
| `audit <copia> [-since D] [-json]` | eventos del daemon y conexiones a Postgres (`ext/db/internal/dbaudit`) |
| `golden [-script P] image \| build ...` | ejecuta `scripts/db-golden.sh` con el mismo `kling` y el mismo daemon |

Todos aceptan `-H` (daemon) y `-owner` (por defecto `local`).

## Etiquetas

Todas cumplen `api.KeyPattern` (sin `/`):

| etiqueta | valor |
|---|---|
| `kling.db.golden` | plantilla de la que sale la copia |
| `kling.db.owner` | quién la pidió; `local` en el CLI |
| `kling.db.state` | `preparing` o `ready` |
| `kling.db.role`, `kling.db.database` | rol y base de la aplicación (`app`, `appdb`) |
| `kind=sandbox` | lo que permite `sandbox fork` sobre la copia |
| `kling.ports` | incluye `5432`: el backend de macOS abre el reenvío |

Las etiquetas **se heredan**: `save` las guarda en la plantilla, `run -from` las
fusiona y `fork` las copia enteras añadiendo `kling.fork-of`. Por eso `up` pasa
`kling.db.state=preparing` en el propio `run -from` (la copia nace así) y `fork`
cambia el `ready` heredado a `preparing` antes de hacer nada más. Y por eso
`connect` no se fía solo de la etiqueta: exige además que exista la contraseña de
**ese id** en este host.

## Credenciales

- Cada copia estrena contraseña antes de marcarse `ready`. La clave (32 bytes
  aleatorios, en hex) se genera en el host; al invitado solo va su **verificador
  SCRAM-SHA-256** (`SCRAM-SHA-256$4096:<sal>$<StoredKey>:<ServerKey>`, RFC 5803/7677,
  `ext/db/internal/scram`), por stdin de `kling exec -i ... psql`, como
  `ALTER ROLE app PASSWORD '<verificador>'`. En la misma sesión se comprueba que
  `pg_authid` guarda ese verificador.
- La clave vive solo en `~/.local/state/kling-db/copies/<id>/password` (0600,
  directorios 0700; `KLING_DB_STATE` cambia la raíz). Va por id y no por nombre: un
  nombre se reutiliza, un id no. `connect` rechaza un fichero que no sea normal, que
  lean otros o que sea de otro usuario.
- Nunca en argv, ni en etiquetas, ni en stdout salvo `connect -dsn` (y en una
  terminal, solo tras confirmar). `-psql` la pasa por el entorno del hijo.
- Si la rotación falla (o Postgres no arranca en 30 s), la copia **se destruye**.
- La contraseña de la plantilla sigue viva en una copia solo mientras está en
  `preparing` (el tiempo de un `pg_isready` y un `ALTER ROLE`), nunca en una `ready`.

## Linux y macOS no son iguales

- **Linux**: el namespace de red de cada máquina DNATea todos los puertos de su IP
  (`netns_fc.go`), así que **cualquier proceso del host** llega a `IP:5432`. No hay
  frontera en la red: la protección es que cada copia tiene su propia clave y esa
  clave solo está en el fichero 0600 de quien la creó. Postgres ve llegar al host
  desde la IP del veth del host.
- **macOS**: el host llega por el reenvío que el backend abre en loopback para
  `kling.ports` (rango reservado 29000-29999), con comprobación del usuario del
  otro extremo (peercred). Postgres ve llegar al host desde `172.16.0.1`.
- En ninguno de los dos llega nada desde `127.0.0.1` del invitado: `pg_hba` solo
  admite al rol de la app por red con `scram-sha-256` y al superusuario `postgres`
  por el socket local y como el usuario del sistema `postgres` (peer).

## Dentro de la copia

El agente, que es root en su microVM, entra como superusuario por el socket:
`su -s /bin/sh postgres -c 'psql -h /run/postgresql appdb'`. `pg_hba` no deja al rol
`app` entrar por el socket; por red (`172.16.0.2:5432`) necesitaría la clave, que no
está en el invitado.

## Límites de este MVP

- `golden` necesita el script: desde un checkout de kindling con `-script`, o
  `KLING_DB_GOLDEN_SCRIPT`, o instalado junto al binario. No busca en el directorio
  actual a propósito.
- `fork` de una copia congelada la deja en marcha.
- Si `sandbox fork` devuelve algo que no se entiende, las copias que hubiera
  quedan con el `ready` heredado pero sin contraseña aquí: `connect` las rechaza y
  hay que borrarlas con `kling rm`.
- `rm` desde fuera (`kling rm`, ttl con `-on-ttl remove`) deja el fichero de la
  contraseña de una máquina que ya no existe; no sirve para ninguna otra (va por id).
