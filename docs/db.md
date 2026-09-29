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
| `fork <copia> [-n N]` | descongela si hace falta, `sandbox fork -label kling.db.state=preparing` (las copias nacen en `preparing`), rota la clave de cada una y las marca `ready`. Todo o nada |
| `connect <copia> [-role R] [-dsn \| -psql]` | sin flags: dirección, usuario, base y la ruta del fichero de la clave. `-dsn`: el DSN con la clave (pregunta si stdout es una terminal). `-psql`: abre el psql del host con la clave en `PGPASSWORD`. `-role R`: como un rol creado con `role` |
| `role <copia> -ro [-name agent] [-schemas a,b] [-timeout 5s] [-rm]` | crea (o con `-rm` borra) un rol de LOGIN de solo lectura dentro de la copia, con su propia clave en el host (`copies/<id>/<rol>.password`, 0600) |
| `reset <copia>` | `rm` + `up` de la misma plantilla, con el mismo nombre, dueño y ttl |
| `rm <copia>...` | borra la máquina y, después, su contraseña |
| `doctor <copia> \| -url postgres://...` | diagnóstico de seguridad (reglas DB001-DB054, `ext/db/internal/doctor`); sale con 1 si hay problemas (todo lo que no es `INFO`) |
| `audit <copia> [-since D] [-json]` | eventos del daemon y conexiones a Postgres (`ext/db/internal/dbaudit`); sin SQL ni claves |
| `golden [-script P] image \| build ...` | ejecuta `scripts/db-golden.sh` con el mismo `kling` y el mismo daemon |
| `golden build -template T <nombre>` | como `build`, con las migraciones y el seed de una plantilla incluida (`empty`, `crm-demo`) |
| `templates` | lista las plantillas incluidas (embebidas en el binario, `ext/db/templates`) |

Todos aceptan `-H` (daemon) y `-owner` (por defecto `local`).

## Rol de solo lectura

`kling db role <copia> -ro` da a un agente o a una herramienta de análisis acceso a la
base sin darle el rol de la aplicación. El rol es `LOGIN NOSUPERUSER NOBYPASSRLS
NOCREATEDB NOCREATEROLE NOREPLICATION`, con `CONNECTION LIMIT 5`, sin pertenencia a
ningún rol (ni `pg_read_server_files` ni `pg_execute_server_program`), `USAGE` en los
esquemas pedidos (por defecto todos menos los del sistema) y `SELECT` en sus tablas,
también en las futuras del rol de la aplicación (`ALTER DEFAULT PRIVILEGES`). Además
`default_transaction_read_only=on`, `statement_timeout` e
`idle_in_transaction_session_timeout` (`-timeout`, 5 s por defecto).

- La barrera real es que **no tiene privilegios de escritura**; la bandera de solo
  lectura se puede apagar con `SET`, los permisos no. Límites: puede crear tablas
  temporales si apaga la bandera (`TEMP` es de PUBLIC), y las tablas que cree un
  superusuario a mano no entran en el `SELECT` futuro.
- La clave sigue la regla de siempre: se genera en el host, al invitado va solo el
  verificador SCRAM por stdin y se comprueba en la misma sesión (junto con los
  atributos y la ausencia de pertenencias). Vive en `copies/<id>/<rol>.password`.
- `pg_hba.conf` de la golden solo deja entrar por red al rol de la aplicación: `role`
  añade una línea `scram-sha-256` para el nuevo rol **después** de comprobarlo, y la
  quita al borrarlo. Si algo falla, se deshace todo (rol, línea y clave).
- Nombres: `^[a-z_][a-z0-9_]{0,62}$`, sin `postgres`, sin el rol dueño, sin `pg_*` ni
  palabras reservadas. `-rm` solo toca roles que creó este comando (comentario
  `kling-db:ro`).
- `kling db connect <copia> -role agent` usa esa clave. `kling db rm` se lleva todas.

## Plantillas

`kling db templates` lista las incluidas y `kling db golden build -template crm-demo
crm` construye una golden de un comando (escribe la plantilla a un temporal 0700 y se
la pasa a `db-golden.sh` como `-migrations` y `-seed`; excluye `-migrations`, `-seed`
y `-seed-mb`). `empty` es una base vacía; `crm-demo` un CRM pequeño (clientes,
contactos, oportunidades con estados, actividades, productos y facturas) con ~50k
filas sintéticas y deterministas, sin datos personales reales.

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
`kling.db.state=preparing` en el propio `run -from` y `fork` lo pasa con `-label` a
`sandbox fork`: en los dos casos la copia **nace** en `preparing`, sin ninguna ventana
en la que exista como `ready` heredado (el núcleo fusiona las etiquetas del fork en el
nacimiento de cada copia). Por eso
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

## Modelo de seguridad

- **Misma microVM.** La base y el agente comparten máquina: el agente es root y
  puede leer todo lo que hay dentro, incluidos los datos y el `postgres`
  superusuario del socket. La frontera es la microVM, no Postgres: lo que una copia
  contiene solo debe ser lo que su agente puede ver. Para separar agente y datos,
  usa una base fuera (proxy de credenciales, [postgres.md](postgres.md)).
- **Rotación obligatoria.** Una copia sale de una plantilla que tiene una clave
  conocida por quien la construyó. Ninguna copia se entrega sin cambiarla: la copia
  se destruye si la rotación falla. Es lo que hace inútil, sobre todo en Linux, que
  cualquier proceso del host alcance el puerto.
- **La clave solo en el host**, en un fichero 0600 por id de máquina; al invitado solo
  va el verificador SCRAM. Nunca en argv, etiquetas, logs ni en la salida de `audit`
  o `doctor` (`connect -dsn` es la única salida que la imprime, a propósito).
- **`doctor`** busca lo que rompe este modelo: roles de login superusuario o con
  `BYPASSRLS`/`CREATEROLE`, políticas RLS que fallan abiertas, clave que sigue siendo
  la de la plantilla (DB052, crítica), fichero de clave ausente o demasiado abierto.
- **`audit`** junta los eventos del daemon y las conexiones que Postgres registra
  (`log_connections`/`log_disconnections`): quién entró, desde dónde y cuándo, no qué
  consultó.
- **Credenciales del proxy.** Una copia con credenciales del proxy no se ramifica
  (cada copia despertaría con marcadores que su proxy no conoce).

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

- No hay TLS entre el host y la copia (`sslmode=disable`): el camino es el veth o el
  reenvío de loopback del propio equipo.
- El agente de la misma microVM ve la base entera (ver Modelo de seguridad).
- La asimetría Linux/macOS de arriba es real: en Linux la clave es la única barrera
  de red; en macOS se suma la comprobación de peercred del reenvío.

- `golden` necesita el script: desde un checkout de kindling con `-script`, o
  `KLING_DB_GOLDEN_SCRIPT`, o instalado junto al binario. No busca en el directorio
  actual a propósito.
- `fork` de una copia congelada la deja en marcha.
- Si `sandbox fork` devuelve algo que no se entiende, las copias que hubiera
  quedan con el `ready` heredado pero sin contraseña aquí: `connect` las rechaza y
  hay que borrarlas con `kling rm`.
- `rm` desde fuera (`kling rm`, ttl con `-on-ttl remove`) deja el fichero de la
  contraseña de una máquina que ya no existe; no sirve para ninguna otra (va por id).

## Prueba de extremo a extremo

`scripts/90-e2e.sh` (sección 7e, lab Linux) y `scripts/92-e2e-mac.sh` (sección 6f, Mac)
recorren `up`, `fork -n 4`, `connect -dsn`, `doctor`, `audit`, `reset` y buscan cada
clave en toda la salida. Sin `KLING_E2E_DB_GOLDEN` (nombre de la plantilla) se saltan,
avisando; `KLING_E2E_DB_GOLDEN_PASSWORD` es opcional y añade la prueba de que la clave
de la plantilla no entra en una copia.
