# kling db: una base Postgres desechable por microVM

`kling db` es la extensión `kling-db` (`ext/db/cmd/kling-db`). Da a un agente o a un
test su propia base Postgres 16, con datos, en milisegundos: una **copia** es una
microVM instanciada de una plantilla con Postgres ya caliente
([db-golden.md](db-golden.md)). Por defecto la base y el agente viven en la **misma**
microVM; con `attach` (el [modelo A](#modelo-a-una-copia-compartida-attach)) una copia
se comparte con agentes de **otras** microVMs sin que vean la contraseña.

**MySQL/MariaDB**: `up`, `fork`, `connect` (`-mysql`, DSN `mysql://`), `rotate`, `reset`,
`rm`, `branch`, `doctor` y `audit` funcionan igual sobre copias de una plantilla MariaDB
(`golden image|build -engine mysql`, etiqueta `kling.db.engine=mysql`); al invitado va
el hash de `mysql_native_password`, nunca la clave. Lo demás se rechaza en copias MySQL
en esta versión. Todo en [mysql.md](mysql.md).

```sh
kling db golden -script scripts/db-golden.sh build -seed-mb 20 pg   # la plantilla, una vez
kling db up pg -name t1                  # una copia lista, con su propia contraseña
kling exec t1 -- su -s /bin/sh postgres -c 'psql -h /run/postgresql appdb'
kling db connect t1 -psql                # desde el host
kling db fork t1 -n 4                    # 4 copias de t1 tal como está ahora
kling db reset t1                        # t1 vuelve a salir de la plantilla
kling db rm t1
kling db doctor t1        ·   kling db audit t1 -since 1h
kling db attach agente t1 -role agent    # otro agente, otra microVM, por el proxy (Linux)
```

## Subcomandos

| comando | qué hace |
|---|---|
| `up <plantilla> [-name N] [-ttl D] [-owner T]` | `run -from` con `kling.db.state=preparing`, espera a Postgres, quita los roles de `role` heredados, **rota la contraseña** y marca `ready` |
| `fork <copia> [-n N]` | descongela si hace falta, `sandbox fork -label kling.db.state=preparing` (las copias nacen en `preparing`), quita en cada una los roles de `role` heredados, rota su clave y las marca `ready`. Todo o nada |
| `connect <copia> [-role R] [-dsn \| -psql]` | sin flags: dirección, usuario, base y la ruta del fichero de la clave. `-dsn`: el DSN con la clave (pregunta si stdout es una terminal). `-psql`: abre el psql del host con la clave en `PGPASSWORD`. `-role R`: como un rol creado con `role` |
| `attach <agente> <copia> [-role R] [-env PGPASSWORD] [-database appdb] [-host H]` | da a un agente de **otra** microVM acceso a la copia por su proxy de credenciales: recibe un marcador en `-env` y el proxy, en el host, pone la contraseña. Solo Linux; ver [Modelo A](#modelo-a-una-copia-compartida-attach) |
| `detach <agente> <copia> [-env PGPASSWORD]` | retira ese acceso y corta sus sesiones abiertas (acepta el id de una copia ya borrada) |
| `role <copia> -ro [-name agent] [-schemas a,b] [-timeout 5s] [-rm]` | crea (o con `-rm` borra) un rol de LOGIN de solo lectura dentro de la copia, con su propia clave en el host (`copies/<id>/<rol>.password`, 0600) |
| `branch [<rama>] [-from P] [-golden G] \| -switch [-golden G] \| -ls \| -rm R \| -prune \| [-owner T] [-golden G] hook install\|uninstall` | una base por rama de git; ver [Una base por rama](#una-base-por-rama) |
| `reset <copia>` | `rm` + `up` de la misma plantilla, con el mismo nombre, dueño y ttl |
| `rm <copia>...` | borra la máquina y, después, su contraseña |
| `rehearse <copia\|golden> -migrations DIR [-lock-timeout 5s] [-keep] [-json]` | ensaya migraciones SQL en una copia desechable: tiempos, esperas por locks y tamaño; ver [Operaciones](#operaciones-rehearse-rotate-snapshot-undo) |
| `rotate <copia>` | clave nueva para la copia; si falla, la vieja sigue valiendo |
| `snapshot [-rm] <copia> <nombre>`, `snapshots <copia>`, `undo <copia> [<nombre>]` | puntos de restauración de una copia viva y vuelta a uno de ellos (mismo nombre y dueño, clave nueva) |
| `doctor <copia> \| -url postgres://...` | diagnóstico de seguridad (reglas DB001-DB054, `ext/db/internal/doctor`); sale con 1 si hay problemas (todo lo que no es `INFO`) |
| `tenant-check <copia> [-role R] [-column tenant_id] [-setting app.tenant_id] [-max 5] [-json]` | prueba que cada inquilino solo ve sus filas y que sin inquilino no ve ninguna; sale con 1 si algo falla. Ver [Aislamiento entre inquilinos](#aislamiento-entre-inquilinos-tenant-check) |
| `diff <copia1> <copia2> [-json] [-schema-only] [-max-rows 100000]` | diferencias de esquema y, por tabla, filas nuevas, borradas y cambiadas por clave primaria, sin volcar datos. Ver [Diff entre copias](#diff-entre-copias-diff) |
| `audit <copia> [-since D] [-json]` | eventos del daemon y conexiones a Postgres (`ext/db/internal/dbaudit`); sin SQL ni claves |
| `ask <copia> "pregunta" [-role R] [-yes] [-explain -send-data]` | un modelo traduce la pregunta a una SQL que se enseña, se confirma y se ejecuta con un rol de solo lectura en una transacción READ ONLY; ver [db-ask.md](db-ask.md) |
| `golden [-script P] image \| build ...` | ejecuta `scripts/db-golden.sh` con el mismo `kling` y el mismo daemon |
| `golden build -template T <nombre>` | como `build`, con las migraciones y el seed de una plantilla incluida (`empty`, `crm-demo`) |
| `clone <postgres-url> -mask REGLAS [-golden G] [-allow-unmasked] [-strict]` | un golden hecho de una base de producción, con los datos personales enmascarados dentro de una microVM; ver [Copia de producción enmascarada](#copia-de-producción-enmascarada-clone) |
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
  añade una línea `host "<base>" "<rol>" 0.0.0.0/0 scram-sha-256 # kling-db` (solo la
  base de la copia) **después** de comprobar el rol, y la quita al borrarlo. Si algo
  falla, se deshace todo (rol, línea y clave). El fichero es el que dice Postgres
  (`SHOW hba_file`), como en `ask`.
- **El rol no pasa a las copias hijas.** Su verificador viaja en la RAM y el disco de
  la copia: sin más, un `fork`, un `undo` desde un punto guardado con el rol, o un
  `up` de un golden hecho de esa copia lo tendrían, y quien tuviera su clave entraría
  en todas (en Linux cualquier proceso del host llega a la IP de cada una). Por eso
  `up`, `fork` y `undo`, antes de marcar la copia `ready`, cortan las sesiones de todo
  rol con comentario `kling-db:ro`, hacen `DROP OWNED BY` (en la base de la copia y en
  `postgres`) y `DROP ROLE`, quitan de `pg_hba.conf` las líneas `# kling-db` y
  recargan. Si queda alguno, la copia se destruye. Las claves del host van por id, así
  que la copia nueva tampoco tiene fichero: se vuelve a crear con `role`, con otra
  clave. El origen no se toca.
- Nombres: `^[a-z_][a-z0-9_]{0,62}$`, sin `postgres`, sin el rol dueño, sin `pg_*` ni
  palabras reservadas. `-rm` solo toca roles que creó este comando (comentario
  `kling-db:ro`).
- `kling db connect <copia> -role agent` usa esa clave. `kling db rm` se lleva todas.

## Aislamiento entre inquilinos (tenant-check)

`doctor -url` habla TLS: `sslmode` de la URL (`verify-full` por defecto: cadena y nombre
contra las raíces del sistema más `sslrootcert` o `-ca-file`; `-tls-server-name` si el
certificado es de otro nombre, p. ej. tras un forward a `localhost`), con SCRAM-SHA-256-PLUS
si el servidor lo ofrece. `disable` y `require` solo en loopback, salvo `-insecure` (el
informe lo avisa con DB041); `allow` y `prefer` se rechazan. Un servidor sin TLS falla en
vez de degradarse.

`doctor` lee las políticas RLS; `kling db tenant-check <copia>` las **ejercita**. Nace del
fallo real de AuraCRM: una política como

```sql
USING (current_setting('app.tenant_id', true) IS NULL
       OR current_setting('app.tenant_id', true) = ''
       OR tenant_id = current_setting('app.tenant_id', true)::uuid)
```

es correcta con el inquilino fijado, pero una conexión que olvida el `SET` ve a todos.

Qué hace, todo dentro de la copia (`kling exec` + psql por stdin):

1. Como superusuario, descubre las tablas con la columna de inquilino (`-column`, por
   defecto `tenant_id`; se excluyen las de extensiones y las particiones), sus
   políticas, si RLS está activa o forzada, si el rol es su dueño y sus privilegios.
2. En una sola sesión guarda en una tabla temporal los primeros `-max` valores de
   inquilino (5; hasta 100) y, como el rol de la aplicación (el de la copia, o `-role`),
   prueba cada tabla en cada escenario: la variable (`-setting`, por defecto
   `app.tenant_id`) **sin fijar**, **a `''`** y fijada a **cada inquilino**:
   - `select`: cuántas filas ve y cuántas son de otro inquilino. Sin inquilino debe ser
     0 (o un error: falla cerrada); con inquilino, ninguna ajena.
   - `insert-other`: copia una fila visible con el inquilino cambiado por otro de la
     misma tabla; `move-to-other`: mueve una fila suya a otro inquilino;
     `update-others`: toca las filas de otros. Cada una en `BEGIN ... ROLLBACK`; debe
     rechazarla la política (`42501`). Si lo que la para es una restricción (clase `23`,
     p. ej. la clave primaria), la política la dejó pasar: Postgres comprueba el `WITH
     CHECK` antes que las restricciones, así que cuenta como fallo.
3. Informe por tabla (`PASS`/`FAIL`/`ERROR`) con las pruebas que fallan, el porqué
   (RLS apagada, el rol es dueño sin `FORCE ROW LEVEL SECURITY`, rol `SUPERUSER` o
   `BYPASSRLS`, y la política culpable con la regla fail-open de `doctor`: "fail-open
   in USING: `<tenant setting> IS NULL` is true when the setting is missing") y el
   arreglo. `-json` da el informe entero. Sale con **1** si hay algún fallo o error:

```console
$ kling db tenant-check crm-1
kling db tenant-check: crm-1 (database appdb, role app, column tenant_id, setting app.tenant_id)
  tenants tested: 3 (-max 5)
  FAIL   public.accounts  (RLS on, 1 policy)
         fail  no tenant (setting unset): SELECT sees 4 row(s) with no tenant set
         fail  no tenant (setting unset): INSERT of a row for another tenant passed row level security; only a constraint stopped it (23505)
         fail  no tenant (setting unset): UPDATE moving a row to another tenant succeeded (1 row(s), rolled back)
         fail  no tenant (setting unset): UPDATE of rows with no tenant set succeeded (4 row(s), rolled back)
         why:  policy "tenant_isolation" (permissive, ALL): fail-open in USING: <tenant setting> IS NULL is true when the setting is missing, so with no tenant set every row passes
         fix:  make the policy fail closed: current_setting('<var>') without missing_ok (errors when unset), or compare only tenant_id = current_setting(...) with no IS NULL / '' / COALESCE escape
  PASS   public.contacts  (RLS on, 1 policy)
summary: 2 table(s), 1 not passing; 4 failed check(s), 0 error(s), 2 skipped
```

(Con esa política exacta, el caso `''` falla cerrado en Postgres 16: el `''::uuid` da
error antes de que la rama `= ''` abra la puerta. Lo que cuenta es lo que hace la base,
no lo que parece la expresión.)

Garantías:

- **Los valores de inquilino no salen de la base.** La sesión los lee de su tabla
  temporal con `set_config(..., v, false)`; no pasan por el host ni se interpolan en la
  SQL. Tampoco se imprime ninguna fila: solo recuentos y SQLSTATE (`\set VERBOSITY
  sqlstate`: los errores no citan valores).
- Los nombres de tablas y columnas van citados como identificadores; los que tienen
  caracteres de control no se prueban (`ERROR`). `-role`, `-column` y `-setting` pasan
  por un patrón antes de escribirse en la SQL.
- Las escrituras se deshacen siempre (`ROLLBACK`); `statement_timeout` 60 s y
  `lock_timeout` 5 s. Es una copia: los efectos fuera de la transacción de un trigger
  (`NOTIFY`, `dblink`) no se deshacen.

Límites:

- Usa `SET ROLE`, no un login: las políticas ven `current_user` = el rol, pero
  `session_user` es `postgres` y no se aplican los `ALTER ROLE ... SET` del rol.
- Si la variable ya tiene valor al abrir la sesión (`ALTER DATABASE ... SET`,
  `postgresql.conf`), el caso "sin fijar" prueba ese valor y el informe lo avisa.
- Los valores de inquilino se toman de todas las tablas: uno que no cabe en el cast de
  la política de otra tabla (un `int` frente a un `uuid`) da `skip` ("inconclusive"), no
  fallo. Sin otro valor de inquilino en la tabla, las escrituras no se prueban.

En CI, después de `kling db up` y las migraciones:

```sh
kling db tenant-check "$COPY" -json > tenant-check.json   # salida 1 si hay fugas
```

## Diff entre copias (diff)

`kling db diff <copia1> <copia2>` dice qué cambió de la primera a la segunda: "esta
migración añadió 3 columnas y cambió 1.200 filas". Complementa `rehearse` (que ensaya) y
`undo` (que vuelve atrás): compara una copia antes y después, o una copia y su `undo`.

- **Esquema**: tablas nuevas y borradas y, en las comunes, columnas (nombre, tipo,
  `NOT NULL`, si cambió el `DEFAULT`), clave primaria, índices, restricciones, RLS
  (activada/forzada) y políticas. Las definiciones que se enseñan pasan por el mismo
  filtro que `tenant-check`: sin literales (`tenant = '…'`).
- **Filas**: por tabla con la misma clave primaria en las dos copias, cuántas filas son
  nuevas, borradas o cambiadas. Se comparan las columnas comunes, así que añadir una
  columna no marca todas las filas como cambiadas.
- **Sin volcar datos**: dentro de cada copia (`kling exec` + psql por stdin, sesión de
  solo lectura) se calculan dos huellas de 64 bits por fila: la de la clave primaria (md5
  con una **sal aleatoria** de esa ejecución, igual en las dos copias, para que no se
  puedan buscar ids conocidos) y la de la fila entera (con la misma sal: tampoco se
  pueden adivinar filas de pocos valores posibles). Al host solo llegan huellas; ni
  una clave ni un valor salen de la base. Los nombres de tablas y columnas se citan como
  identificadores.
- **Sin clave primaria** (o distinta en cada copia): solo recuentos y un aviso.
- **`-max-rows N`** (100000; hasta 2.000.000): con más filas en una tabla se muestrea, 1
  de cada k según la huella de la clave (las mismas filas en las dos copias), y el
  informe lo declara: los recuentos de esa tabla son los de la muestra.
- **`-schema-only`**: solo el esquema; no cuenta ni lee filas.
- `-json` para máquinas. La salida es 0 aunque haya diferencias; `same` en el JSON dice
  si las copias son iguales.

Las copias deberían estar quietas mientras se comparan: cada tabla se lee en su propia
sesión, no en una instantánea común. Una huella de 64 bits basta para decidir "cambió", no
es una prueba criptográfica de igualdad.

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
| `kling.db.engine` | `mysql` en las copias de una plantilla MariaDB/MySQL (la pone `db-golden-mysql.sh` y se hereda); sin ella, Postgres |
| `kind=sandbox` | lo que permite `sandbox fork` sobre la copia |
| `kling.db.repo`, `kling.db.branch`, `kling.db.used` | `kling db branch`: hash del directorio git común del repo (antes, del toplevel), clave de la rama y último uso (segundos unix) |
| `kling.ports` | incluye `5432` (`3306` en MySQL): el backend de macOS abre el reenvío |

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
- En una copia MySQL, lo mismo con el hash de `mysql_native_password`
  (`ALTER USER 'app'@'%' IDENTIFIED WITH mysql_native_password AS '<hash>'`, al cliente
  de root por el socket) y la comprobación en `mysql.user`; ver [mysql.md](mysql.md).

## Modelo de seguridad

- **Misma microVM.** La base y el agente comparten máquina: el agente es root y
  puede leer todo lo que hay dentro, incluidos los datos y el `postgres`
  superusuario del socket. La frontera es la microVM, no Postgres: lo que una copia
  contiene solo debe ser lo que su agente puede ver. Para separar agente y datos,
  usa el [modelo A](#modelo-a-una-copia-compartida-attach) (la copia en su máquina,
  el agente en otra, el rol como límite) o una base fuera (proxy de credenciales,
  [postgres.md](postgres.md)).
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
  (cada copia despertaría con marcadores que su proxy no conoce). Tampoco un agente con
  `attach`: se hace `detach`, se ramifica y se hace `attach` de cada copia.

## Modelo A: una copia compartida (attach)

Varios agentes, cada uno en su microVM, trabajan contra **una** copia que vive en la
suya. Ninguno ve la contraseña: cada uno recibe un marcador y conecta al proxy de
credenciales de su máquina, que marca a la copia con la clave. En Linux y en macOS
(allí por el broker de enlaces del daemon; ver abajo).

```sh
kling db up pg -name crm                           # la copia
kling db role crm -ro -name agent                  # un rol de solo lectura (recomendado)
kling run -image toolchain -name a1 -egress allowlist -allow example.org -allow-exec
kling db attach a1 crm -role agent                 # a1: PGPASSWORD = marcador
# dentro de a1 (PGPASSWORD = el marcador de MMDS env):
#   psql "host=crm.db.internal user=agent dbname=appdb sslmode=disable"
kling db detach a1 crm                             # fuera, y sus sesiones cortadas
```

- **Qué se entrega.** Al agente, una credencial Postgres del proxy
  (`POST /machines/{ref}/credentials`) con `upstream_machine` = el **id** de la copia,
  `upstream_owner` = el dueño, `upstream_tls: disable` (SCRAM-SHA-256) y la clave del rol
  (la de la aplicación de `copies/<id>/password` o, con `-role`, la de
  `copies/<id>/<rol>.password`; nunca la del golden). El agente conecta a
  `<copia>.db.internal` (`-host` para otro nombre), que su resolver desvía al proxy.
  La clave va en el cuerpo de la petición al daemon (ni argv ni ficheros) y se guarda
  cifrada en el almacén del agente, como cualquier credencial.
- **Dirección en cada conexión, no al entregar.** Lo guardado es el id, no una IP: un
  índice de red se reutiliza cuando la copia se para o se borra, y una dirección fijada
  llevaría la clave al invitado de otro. En **cada** conexión el proxy pregunta al
  daemon, que bajo su candado exige que la copia con ese id exacto exista, corra, esté
  `ready`, exponga el 5432 en `kling.ports`, y que la copia, el agente y la credencial
  tengan el mismo `kling.db.owner`. Si no, `08006` al agente y `machine_unavailable` en
  su auditoría (`kling machine audit <agente>`, con `upstream: machine:<id>`).
- **Cortes.** Congelar, pausar, parar, borrar o marcar fallida la copia, cambiar sus
  etiquetas de `kling db` o las del agente, o `detach`, cortan en el acto las sesiones
  abiertas. Tras `thaw` el agente vuelve a entrar sin hacer nada.
- **Dueño.** `attach` exige que la copia sea del `-owner` (por defecto `local`) y que el
  agente sea del mismo: si no tiene `kling.db.owner`, se le pone; si tiene otro, se
  rechaza. El daemon lo vuelve a mirar en cada conexión.
- **Lo que rompe un attach.** `reset` y `undo` crean otra copia (otro id), y `rotate`
  cambia la clave de la aplicación: hay que repetir `attach`. Un rol de `role` sobrevive
  a `rotate`. Un agente con `attach` no se ramifica.
- **Límites.** Todos los agentes de una copia ven la misma base: el aislamiento entre
  ellos es el rol (uno de solo lectura por agente con `-role`), no la copia. Las
  consultas van en claro por el host entre el proxy y la copia (el veth en Linux, el
  loopback en macOS).
- **En macOS.** El proxy de Postgres de cada máquina lo sirve su `kling-vz`, confinado
  y sin conocer las demás, y no recibe nunca una dirección: en cada conexión pide la
  copia al **broker de enlaces** del daemon (un socket Unix privado del usuario; el
  daemon sabe qué máquina pregunta por el PID del otro extremo). El daemon hace las
  mismas comprobaciones de arriba, comprueba además que el agente tiene esa credencial,
  marca él mismo al reenvío de la copia y le entrega el socket ya conectado. Los cortes
  son los mismos: el daemon guarda su copia del socket y la cierra en los dos lados.
  Hace falta un `kling-vz` que anuncie `graph-link` en `credential_kinds` (uno
  anterior no recibe la credencial). Ver [SECURITY.md §15](../SECURITY.md).

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

`scripts/90-e2e.sh` (sección 7e, lab Linux; la 7f prueba `attach`: dos agentes leen la
misma copia por el proxy, congelarla corta la sesión abierta y el siguiente intento, el
thaw la devuelve, otro dueño no entra y `detach` retira el acceso) y
`scripts/92-e2e-mac.sh` (sección 6f, Mac)
recorren `up`, `fork -n 4`, `connect -dsn`, `doctor`, `audit`, `reset`, `role -ro`
(INSERT, DELETE, COPY TO PROGRAM y SET ROLE tienen que fallar con ese rol), `rotate` (la
clave vieja deja de valer), `snapshot` + `undo`, `rehearse` (una migración que añade una
columna y otra que se bloquea por `lock_timeout`), `golden build -template crm-demo` (y su
consulta) y `ask` (solo con `ANTHROPIC_API_KEY`: no hay proveedor falso; sin ella se salta,
avisando), y buscan cada clave en toda la salida. La versión de 92 es más corta (sin el
bloqueo, `golden` ni `ask`). Para CI, ver [db-ci.md](db-ci.md). Sin `KLING_E2E_DB_GOLDEN` (nombre de la plantilla) 90 se salta,
avisando; 92 (Mac) construye ella misma la plantilla base (toolchain + `apk add postgresql16`,
único paso con egress internet) y el golden con `db-golden.sh -from`, y compila el plugin
con Go o usa `KLING_DB=ruta`; sin Go o sin red se salta, avisando; `KLING_E2E_DB_GOLDEN_PASSWORD` es opcional y añade la prueba de que la clave
de la plantilla no entra en una copia.

`clone` tiene su subsección, "7e (clone)", con `KLING_E2E_CLONE_ADMIN_URL` (un
superusuario de un Postgres con SCRAM al que lleguen el shell y el daemon, p. ej. un
Docker en el host del lab): crea una base con datos personales falsos y un rol de solo
lectura, comprueba que una columna sospechosa sin regla, un superusuario y una regla
que no cabe no dejan ni golden ni máquina, y que el golden bueno no contiene ningún
valor original pero conserva el join por correo y los `NULL`.

## Operaciones: rehearse, rotate, snapshot, undo

### `kling db rehearse <copia|golden> -migrations DIR [-lock-timeout 5s] [-keep] [-json]`

Ensaya migraciones en una copia desechable: un fork de una copia lista (que
debe estar en marcha: rehearse no descongela ni toca el origen) o un up de un
golden. Aplica los `.sql` de DIR en orden de nombre, como el rol de la
aplicación, por stdin y con `ON_ERROR_STOP`, y mide
por fichero: duración, tamaño de la base antes y después y bloqueos. Termina
con una tabla (o `-json`) y destruye la copia salvo `-keep`. Si un fichero
falla, los siguientes se marcan `skipped` y el código de salida es 1.

Los bloqueos, sin adornos: en una copia aislada nadie más toma locks, así que
se ve lo que la migración provoca por sí sola. Se detecta con un muestreo de
`pg_stat_activity` (`waited ~N s`) y con el fallo por `lock_timeout`, que se
informa como `would block N s in production`. Sirve para descubrir qué
sentencias piden un lock fuerte, no para predecir la espera real en producción.
El error de un fichero muestra solo la línea `ERROR:` de psql (el resto cita la
sentencia). Los `.sql` no pueden ser enlaces simbólicos.

Como qué rol, con precisión: el psql de la migración **entra como el rol de la
aplicación** por el socket (`-U app`, peer con un mapa `kling_db_rehearse` que
permite al usuario del sistema `postgres` ser `app`, lo mismo que usa `ask`). No
entra como `postgres` con `role=app`: ahí un `RESET ROLE` en un `.sql` lo volvería
superusuario, y el ensayo no se parecería a producción. Así `session_user` es
`app` y ni `RESET ROLE` ni `SET ROLE postgres` suben de privilegios. Lo que **no**
se cierra: los metacomandos de psql (`\!`, `\copy ... program`) ejecutan órdenes
en el invitado como el usuario del sistema `postgres`, que entra como superusuario
por el socket. Las migraciones son código de confianza; lo que limita el daño es
que la copia es desechable y es una microVM propia.

### `kling db rotate <copia>`

Contraseña nueva con el mismo mecanismo que `up` (verificador SCRAM por stdin).
Atómico: la nueva se deja en `password.new`, se cambia la base y solo entonces
pasa a ser `password`. Si algo falla, el fichero no cambia y se devuelve a la
base el verificador de la clave anterior. Las sesiones abiertas siguen hasta
que reconectan.

### `kling db snapshot <copia> <nombre>`, `snapshots <copia>`, `undo <copia> [<nombre>]`

Un punto de guardado es una plantilla propia (`kling save` de la máquina viva)
llamada `dbsnap-<hash dueño+copia>-<nombre>`, con `kling.db.golden` (el golden
original), `kling.db.owner` y `kling.db.snapshot-of` (el id de la copia; una
copia nacida de un undo lleva el mismo valor, así conserva sus puntos). El
nombre es `[a-z0-9-]`, hasta 40; máximo 16 puntos por copia.

- `snapshot` exige copia lista y propia, sin conexiones de cliente abiertas
  (sus sockets se repartirían idénticos a toda copia nacida del punto; `-force`
  lo salta) y hace CHECKPOINT antes. No pisa puntos: repetir un nombre falla.
- `undo` borra la copia y crea otra con el mismo nombre, dueño y ttl desde el
  punto (el último si no se da nombre); el punto no se consume. Si el `up`
  posterior falla, la copia ya no existe y el error dice cómo recrearla desde
  la plantilla.
- `snapshot -rm <copia> <nombre>` borra el punto; kindling se niega mientras
  haya copias vivas nacidas de él (un undo deja una).
- Los puntos de una copia borrada con `kling db rm` no se borran solos:
  quítelos antes con `snapshot -rm`.

**Contraseña.** El punto guarda la RAM y el disco de ese momento, incluido el
verificador de la contraseña que la copia tenía entonces. `undo` no la
recupera: la copia nueva rota una propia antes de darse por lista, y el fichero
del host es de la copia nueva. La clave del punto solo sirve, en teoría, para
esa plantilla; la rotación la invalida en cuanto nace la copia. Los roles de
`kling db role` que tuviera la copia tampoco vuelven (ver Rol de solo lectura).

**Los puntos son plantillas globales de kindling, sin dueño.** El nombre
`dbsnap-...` y la etiqueta `kling.db.owner` sirven para que `snapshots` y `undo`
encuentren los de cada copia, no son un control de acceso: quien tenga el socket
del daemon (o `-H` a él) puede listarlos con `kling template ls` y hacer
`kling db up dbsnap-...`, igual que con cualquier golden. La copia que salga
rota su propia clave y pierde los roles de `role`, pero **los datos** del punto
son los que eran. Mismo modelo que los golden: el daemon es la frontera; no
guardes puntos de datos que no deba ver quien lo usa.

## Una base por rama

`kling db branch` da a cada rama de git su propia copia. La de una rama nueva sale
por `fork` de la copia de su rama padre (los datos y el esquema de esa rama, en
milisegundos) o, si el padre no tiene copia lista, por `up` del golden.

```sh
kling db branch -golden crm-demo      # rama actual: crea su copia (fork del padre si existe)
kling db branch feat/x -from main     # la de otra rama, ramificada de la de main
kling db branch hook install          # a partir de aquí, cada `git checkout <rama>` cambia de base
kling db branch -ls                   # ramas con su copia, estado, tamaño y último uso
kling db branch -rm feat/x            # borra la copia (y su clave) de una rama
kling db branch -prune [-dry-run]     # borra las copias de ramas que ya no existen en git
```

- **Rama y padre.** Sin argumento, la rama actual (`git symbolic-ref`; en HEAD suelto
  se niega). El padre es `-from` o la rama por defecto del repo (`origin/HEAD`, o
  `main`). Sin copia del padre ni `-golden`, se usa el golden de cualquier otra copia
  del repo; si no hay ninguno, falla y pide `-golden`.
- **`-switch`** es lo que llama el hook. Deja **activa** la copia de la rama actual
  (`thaw` si estaba congelada, ~10 ms) y **congela** las de las demás ramas del repo
  (0 RAM). Escribe la conexión en `.git/kling-db.env` (0600). Antes de nada borra el
  fichero de la rama anterior: si algo falla, la aplicación no conecta a la base de
  otra rama por error.
- **La conexión** (`DATABASE_URL`, `PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`,
  `PGPASSWORD`) vive **dentro de `.git`**, nunca en el árbol de trabajo: no se puede
  subir al repo por descuido. Cárguela con
  `set -a; . "$(git rev-parse --absolute-git-dir)/kling-db.env"; set +a`
  (o `direnv`, `--env-file`...).
- **El hook** `post-checkout` solo actúa en cambios de rama (no en `git checkout --
  fichero`), es idempotente y **no pisa** uno existente: lo aparta a
  `post-checkout.pre-kling-db` y lo ejecuta primero (`uninstall` lo restaura). Nunca
  bloquea el checkout: si `kling db branch -switch` falla, avisa por stderr y el
  checkout sigue. Usa `${KLING:-kling}`; para otro daemon, exporte `KLING_HOST`. Con
  `kling db branch -owner equipo -golden crm-demo hook install`, el hook llama a
  `-switch -owner equipo -golden crm-demo`: el dueño de las copias y el golden del que
  sale una rama sin copia del padre quedan escritos en el hook (validados y entre
  comillas simples). Reinstalar sin ellos vuelve al hook de siempre.
- **Worktrees.** `kling.db.repo` es el hash del **directorio git común**
  (`git rev-parse --git-common-dir`), el mismo para el árbol principal y todos sus
  `git worktree`: una rama tiene **una sola copia** aunque se abra desde varios
  worktrees, y el hook (que vive en el directorio de hooks común) sirve a todos. Cada
  worktree escribe su propia conexión en **su** directorio git
  (`.git/worktrees/<nombre>/kling-db.env`; la ruta la da
  `git rev-parse --absolute-git-dir`). `-switch` no congela la copia de una rama que
  otro worktree tiene activa (`git worktree list`): solo las de ramas que nadie usa.
  Las copias creadas antes de este cambio llevan el hash del toplevel y se siguen
  reconociendo desde el árbol que las creó.
- **Un cerrojo por repositorio.** Todo lo que crea, activa o borra copias de rama
  (`branch`, `-switch`, `-env`, `-rm`, `-prune`) toma antes un `flock` sobre
  `$KLING_DB_STATE/locks/branch-<repo>.lock` (0600, en un directorio 0700). Dos
  checkouts a la vez —dos worktrees, o el hook y un comando— se ponen en fila: el
  segundo espera (hasta 3 minutos, avisando por stderr) y encuentra la copia hecha en
  vez de crear otra. El cerrojo lo suelta el sistema si el proceso muere. Es por host:
  dos máquinas contra el mismo daemon no se ven.
- **Identidad y nombres.** La copia se reconoce por sus etiquetas, no por su nombre
  (`sandbox fork` no deja ponerlo): `kling.db.repo` es el hash del directorio git común
  del repo (ver Worktrees) y
  `kling.db.branch` una **clave** de la rama (slug ASCII más 6 hex del hash del nombre
  entero: `feat/x` y `feat-x` no chocan). El nombre de la rama es texto arbitrario y
  nunca va sin validar a argv, a SQL ni a etiquetas; se rechaza el vacío, un `-` inicial,
  los caracteres de control, el UTF-8 inválido y más de 255 bytes.
- **Seguridad.** La clave de cada copia sigue solo en el host (`dbstate`), y cada copia
  de rama rota la suya al nacer, como en `fork`. Solo se listan, tocan y borran copias
  del `-owner` indicado. `-prune` se niega si git no dice ninguna rama local.
- Cada rama viva cuesta disco (su overlay), no RAM: `-prune` y `-rm` lo recuperan.

## Copia de producción enmascarada (clone)

`kling db clone` construye un golden a partir de una base de **producción** con los
datos personales enmascarados: para probar con datos realistas sin que nadie vea un
correo, un nombre o una tarjeta de verdad.

```sh
cat > mask.yaml <<'EOF'
# tabla.columna (esquema public) o esquema.tabla.columna: tipo
users.email: email
users.full_name: name
users.phone: phone
users.username: keep          # no es un dato personal: se deja a propósito
billing.cards.number: card
users.notes: "null"
users.country: "fixed:ES"
orders.customer_email: email  # el mismo correo, el mismo resultado: el join sigue casando
EOF

printf '%s' "$PROD_RO_PASSWORD" | kling db clone -password-stdin -mask mask.yaml -golden shop-masked \
  'postgres://readonly@db.prod.example.com:5432/shop'
kling db up shop-masked -name t1          # copias como de cualquier golden
```

La contraseña va en `PGPASSWORD` o por stdin con `-password-stdin`; una URL con
contraseña se rechaza (acabaría en `ps` y en el historial).

### Qué pasa, en orden

1. **Máquina de construcción** `<golden>-clone-<azar>` desde `pg16` (o `-from`, en macOS)
   con `-egress allowlist`, `-ttl 3h -on-ttl remove` (si `kling-db` muere, la máquina se
   **borra**, no se congela: congelarla guardaría su RAM en disco) y `-mem 2G` (`-mem`).
2. **Credencial al proxy.** La contraseña se entrega al proxy de credenciales de
   Postgres de esa máquina ([postgres.md](postgres.md)): `-domain
   source.clone.internal`, `-upstream <host:puerto de la URL>` (vale una base de la LAN o
   del propio host) y, por defecto, **TLS verify-full** contra el host de la URL
   (`-tls-server-name` lo cambia, `-ca` añade una CA). `?sslmode=disable` en la URL
   quita el TLS hacia el servidor y entonces el proxy exige SCRAM-SHA-256. El invitado
   solo ve el marcador (lo lee de MMDS); la contraseña no entra en la microVM.
3. **Postgres de preparación en RAM.** Un segundo cluster con su directorio de datos en
   un **tmpfs** del invitado (`/run/klingclone`), solo por socket local (sin TCP) y sin
   fsync. Si el invitado tuviera swap, se niega.
4. **Comprobación del rol.** Por el proxy: si el rol es superusuario, se para; si puede
   escribir (`INSERT`, `UPDATE`, `DELETE` o `TRUNCATE`) en alguna tabla, se para salvo
   `-allow-writer`. También se para si el servidor es más nuevo que Postgres 16 (el
   `pg_dump` de la imagen no sabría volcarlo).
5. **Volcado dentro de la microVM.** `pg_dump --no-owner --no-privileges --no-blobs ...`
   por el proxy, en una **tubería** al psql del cluster del tmpfs: el volcado sin
   enmascarar no se escribe en ningún fichero, ni del invitado ni del host.
6. **Catálogo, plan y bloqueo.** Se leen tablas, columnas y recuentos (sin valores) y se
   cruzan con las reglas. Una regla que no casa con ninguna columna es un error (una
   errata dejaría una columna sin tratar); una columna **sospechosa por su nombre** sin
   regla **bloquea** la construcción (ver abajo).
7. **Enmascarado** en una transacción del cluster del tmpfs, con
   `session_replication_role = replica` (ni claves foráneas ni disparadores del usuario:
   un disparador de auditoría no copia los valores viejos a otra tabla), un `UPDATE` por
   tabla y la comprobación de que **ninguna** columna enmascarada conserva un valor
   viejo. Si una regla no cabe en su columna (un correo en un entero), si choca una
   restricción única o si queda un valor viejo, la transacción se deshace y **no queda
   golden**.
8. **El golden es otra máquina.** Se vuelca el cluster ya enmascarado, la máquina de
   construcción se destruye y `db-golden.sh build -seed <volcado>` construye el golden
   en una máquina **nueva** con `egress none`, como cualquier otro golden (rol `app`,
   base `appdb`, clave solo en el host). El golden nunca ha visto un dato sin
   enmascarar, ni tiene ruta a producción, ni hereda la credencial.
9. **Informe** por stdout (`-json` en JSON): columnas tratadas con su tipo y cuántas
   filas cambiaron, las que se dejaron con `keep`, las sospechosas y qué se hizo con
   cada una. **Nunca valores**: tampoco el de las reglas `fixed`.

Si cualquier paso falla, se borran la máquina de construcción y el volcado, y el golden
anterior con ese nombre (si lo había) sigue intacto. Un golden existente no se pisa sin
`-replace`.

### Las reglas

| tipo | valor nuevo (los `NULL` siguen siendo `NULL`) |
|---|---|
| `email` | `user_<16 hex>@example.invalid` |
| `name` | `Person <8 HEX>` |
| `phone` | `+1555` y 7 cifras (sin `+` en una columna numérica) |
| `card` | `9999` y 12 cifras (un prefijo que no emite ninguna red) |
| `text` | `text_<16 hex>` |
| `null` | `NULL` en todas las filas (falla si la columna es `NOT NULL`) |
| `keep` | la columna se copia tal cual, a propósito (y el informe lo dice) |
| `fixed:<valor>` | el mismo valor en todas las filas con valor |

Los tipos con hash usan `sha256(sal ‖ valor)`: la **sal** son 256 bits al azar de
cada construcción, que viajan solo en el SQL del enmascarado (por stdin a la máquina de
construcción) y se tiran al acabar. Dentro de una construcción, el mismo valor da el
mismo resultado en cualquier tabla y columna del mismo tipo: `users.email` y
`orders.customer_email` siguen casando, y una clave foránea sobre columnas enmascaradas
con el mismo tipo sigue siendo válida. Entre dos construcciones, no (otra sal). El valor
nuevo pasa por `CAST` al tipo de la columna: en un `varchar(n)` corto se trunca.

Formatos del fichero: un objeto JSON plano (`{"users.email": "email"}`) o YAML simple,
una regla por línea (`users.email: email`, comillas opcionales, `#` comenta). Una columna
repetida, un tipo desconocido o un nombre con caracteres de control se rechazan. Una
tabla particionada lleva la regla en la raíz (una regla sobre una partición es un
error); una columna generada no admite regla (se recalcula de sus entradas).

### Columnas sospechosas

Por su nombre, partido en palabras (`firstName`, `billing_address2`): `email`/`mail`,
`phone`/`tel`/`mobile`, `name` (y `first_name`, `surname`…), `dni`/`nif`/`nie`/`ssn`/
`passport`, `iban`/`bic`, `card`/`pan`/`cvv`, `address`/`street`/`zip`/`postcode`, `ip`,
`birth`/`dob`; y por su tipo, `inet`/`cidr`/`macaddr`. Sin regla, la construcción **se
para** y lista las columnas; `keep` las acepta una a una y `-allow-unmasked` todas (el
informe las marca `LEFT UNMASKED`). Es una heurística que se equivoca hacia el lado de
bloquear (`product_name` es sospechosa). `-strict` hace sospechosa además toda columna de
texto, JSON, XML, `bytea` o array, se llame como se llame: la opción para quien no quiere
que una columna `notes` con texto libre pase sin que nadie la mire.

### Límites

- **La detección es por nombre y tipo, no por contenido.** Una columna `notes` con
  correos dentro pasa sin tratar salvo con `-strict`. Tampoco se miran dentro los JSON:
  a una columna JSON solo le valen `null`, `keep` o `fixed`.
- **No se vuelcan** objetos grandes (`--no-blobs`), publicaciones, suscripciones,
  etiquetas de seguridad ni tablespaces. Las políticas de RLS que nombran roles de
  producción no se restauran (el rol no existe) y paran la construcción. Un rol sujeto a
  RLS necesita `BYPASSRLS` para volcar entero.
- **Tamaño y tiempo.** Todo lo sin enmascarar vive en la RAM de la máquina de
  construcción (`-mem`, el tmpfs es el 75 %), el volcado tiene 1 h (el tope de `kling
  exec`) y el golden cabe en el overlay de 512 MiB de `db-golden.sh`: pensado para bases
  de hasta unos cientos de MiB, o para un esquema recortado.
- **Postgres ≤ 16 en el origen** (el `pg_dump` de la imagen `pg16`).
- **Extensiones.** El volcado enmascarado se carga como el rol `app`: una extensión no
  "trusted" necesita `-as-super` (y entonces los objetos son del superusuario).
- **El volcado enmascarado sí pasa por el host**: un fichero 0600 en un directorio 0700
  mientras `db-golden.sh` lo carga, borrado al terminar. Es lo mismo que acaba en el
  disco del golden.
- **La RAM de la máquina de construcción es memoria del host** (el proceso del VMM): si
  el host tiene swap, el sistema operativo podría llevarla a disco. En Linux, sin swap
  en el host o con swap cifrada.
- Qué garantiza y qué no, en [SECURITY.md](../SECURITY.md) §16.
## Un entorno entero: app + base como grafo

`kling db env` usa los [grafos](grafos.md) del núcleo para levantar de una vez la
aplicación y su copia de base, unidas por una arista `credential`: es el `kling db
attach` de siempre, declarado en el grafo en vez de a mano.

```sh
kling db env up my-app -golden crm-demo -name pr-42 -allow api.stripe.com -app-port 8081
kling db connect pr-42-db -psql       # la base, desde el host
kling db env down pr-42               # el grafo, sus dos máquinas y la clave
kling db branch -env my-app -golden crm-demo   # lo mismo, un entorno por rama de git
```

- **Qué crea.** Un grafo `<name>` con dos nodos: `db` (`from` el golden; el rol, la base,
  el dueño y el estado como en `kling db up`) y `app` (`from` la plantilla de la
  aplicación, `egress allowlist` con los `-allow` que pases). La arista `app -> db` es
  `credential` en el 5432: dentro de `app`, `db.graph:5432` con `PGPASSWORD` (un
  marcador) llega a la base con la clave real puesta por el proxy. La copia de base es
  una copia normal de `kling db`: `connect`, `snapshot`, `undo` y `rotate` la aceptan
  con el nombre `<name>-db`.
- **La clave.** Se genera en el host y llega al daemon por **stdin** de `kling graph up`
  (nunca en argv, en el entorno ni en el fichero del grafo, que va a un directorio
  temporal 0700 y se borra). Al invitado de `db` solo va su verificador SCRAM, por stdin,
  como en `rotate`. Si algo falla (Postgres no arranca, memoria...), se borra el grafo y
  la clave; no queda nada a medias.
- **`-app-port`** expone ese puerto de `app` en el grafo (no el 8080: es el del agente
  de invitado y una arista no puede llegar a él). En macOS la arista `credential` va por
  el broker de enlaces del daemon (ver
  [grafos.md](grafos.md#cómo-llega-un-nodo-a-otro-macos)); hace falta un `kling-vz` que
  anuncie `graph-link`.
- **`branch -env <app-template>`** hace lo mismo para una rama: el grafo se llama
  `e<repo>-<rama>-<hash>` (cabe en los 24 caracteres del núcleo), es idempotente y es
  **aparte** de la copia suelta de la rama: el hook no lo activa ni lo congela. El golden
  es `-golden` o el de cualquier copia del repo. `branch -rm <rama>` lo borra con ella.
  No se ramifica del entorno del padre todavía: cada rama nace del golden. Ramificar un
  entorno vivo es `kling graph fork` (el grafo entero, en un instante consistente).
- Solo se borra (`env down`, `branch -rm`) un entorno cuya copia de base es del `-owner`
  indicado.
