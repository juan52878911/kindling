# kling db ask: preguntas en lenguaje natural a una copia

`kling db ask` es para quien no programa: escribe una pregunta, un modelo la traduce a
**una** consulta SQL, la ves, la confirmas y se ejecuta **en solo lectura** sobre una
copia de [`kling db`](db.md).

```sh
export ANTHROPIC_API_KEY=...            # la clave de la API, solo en el entorno
# o, sin clave de Anthropic, con opencode (MiniMax): kling db ask ... -provider opencode
kling db up pg -name t1
kling db ask t1 "¿cuáles son los 5 clientes que más han gastado este año?"
kling db ask t1 "pedidos por mes en 2025" -json
kling db ask t1 "¿hay algo raro en las devoluciones?" -explain -send-data
```

| flag | qué hace |
|---|---|
| `-role R` | rol de solo lectura con el que ejecutar (por defecto `kling_db_ro`, que se crea si falta) |
| `-yes` | ejecuta sin preguntar (la SQL se enseña igual, por stderr) |
| `-provider P` | `anthropic` u `opencode` (por defecto: `$KLING_DB_ASK_PROVIDER`; si no, `anthropic` si hay `ANTHROPIC_API_KEY`; si no, `opencode` si está instalado; si no, error) |
| `-model M` | modelo (por defecto `claude-sonnet-5` con anthropic, `minimax-coding-plan/MiniMax-M2.7` con opencode) |
| `-llm-timeout D` | plazo de la respuesta del modelo con opencode (90 s; de 1 s a 30 min); el proceso se mata entero al vencer |
| `-limit N` | filas como máximo (200; de 1 a 10000) |
| `-timeout D` | `statement_timeout` de la consulta (30 s; de 1 s a 10 min) |
| `-json` | `{"sql", "role", "columns", "rows", "truncated", "summary"}` en stdout |
| `-explain` | pide al modelo un resumen en palabras de las filas; **exige `-send-data`** |
| `-send-data` | permite que `-explain` mande hasta 50 filas del resultado al proveedor |

Además, `-H` y `-owner` como el resto de `kling db`.

## `kling db ask-web`: la misma garantía en una página web

Para quien no usa la terminal. Se arranca en la máquina del operador y se abre en el
navegador:

```sh
kling db ask-web t1                      # imprime http://127.0.0.1:PORT/?t=<token>
kling db ask-web t1 -listen 127.0.0.1:8765 -provider opencode
```

Acepta `-role`, `-provider`, `-model`, `-llm-timeout`, `-limit`, `-timeout`, `-explain`
y `-send-data` con el mismo significado y las mismas comprobaciones que `ask` (no hay
`-yes`: la web siempre pide el botón). Además:

| flag | qué hace |
|---|---|
| `-listen HOST:PORT` | dirección (por defecto `127.0.0.1:0`, un puerto libre); solo loopback |
| `-allow-remote` | acepta una dirección que no sea loopback, con un aviso: es HTTP sin TLS. Mejor un túnel SSH al loopback |
| `-ttl D` | cuánto vive la página (30 min; de 1 min a 8 h); después responde 410 y el proceso termina |

**Las garantías de `ask` no cambian.** Al modelo solo van el esquema y la pregunta (filas
solo con `-explain -send-data`, y la página avisa de ello); la SQL pasa por `sqlguard`; se
**muestra** y solo se ejecuta cuando el usuario pulsa "Run this query"; se ejecuta con el
rol de solo lectura en `BEGIN TRANSACTION READ ONLY` con `statement_timeout`, envuelta en
`LIMIT`; el resultado es una tabla. La petición de ejecutar lleva el **id** de la
propuesta y `confirm: true`, nunca una SQL: se ejecuta exactamente la que se enseñó (se
vuelve a validar) y cada propuesta vale una vez y caduca a los 10 min. Un error de
validación no deja ninguna propuesta que ejecutar. Sin el reintento de `ask` tras un
"no existe": el usuario simplemente vuelve a preguntar.

**Seguridad de la página:**

- Escucha solo en loopback; otra dirección se rechaza salvo `-allow-remote`. Se comprueba
  la cabecera `Host` (contra DNS rebinding) cuando es loopback.
- El token aleatorio (192 bits) va en la URL impresa por stdout; al abrirla pasa a una
  cookie `HttpOnly; SameSite=Strict` y el navegador es redirigido a `/`, así que no queda
  en la barra ni en el historial. Sin la cookie, todo es 403.
- Cada POST exige además un segundo token (CSRF, distinto del de la URL, que solo lleva la
  página ya autenticada) en `X-CSRF`, `Content-Type: application/json`, y rechaza un
  `Origin` o `Sec-Fetch-Site` de otro sitio. Los tokens se comparan en tiempo constante.
- CSP estricta sin `unsafe-inline` (`default-src 'none'; script-src 'self'; style-src
  'self'`): el JS y el CSS son ficheros propios embebidos en el binario, sin dependencias
  ni CDN, y lo que viene del modelo o de la base entra con `textContent`, nunca como HTML.
  Además `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Cache-Control: no-store`. Ni CORS ni preflight.
- Límites: cuerpos de 16 KiB, 30 peticiones por minuto y 500 en total, una consulta a la
  vez, 20 propuestas pendientes como mucho y tiempos de lectura y escritura del servidor.
- La clave de la copia no está en ninguna respuesta: no se lee en la página, y si aparece
  en una celda, un resumen o un error se sustituye por `[redacted]`.
- Con `-allow-remote` cualquiera que llegue al puerto Y tenga la URL puede consultar la
  copia (con el mismo rol de solo lectura): trátala como una clave.

**Probar sin clave de un proveedor.** `KLING_DB_ASK_FAKE=<fichero>` (ver arriba) también
vale para `ask-web`: el "modelo" contesta siempre lo que hay en el fichero, y todo lo demás
(validación, botón, rol, transacción) es real. Sirve para el e2e y para ver la página sin
red. La prueba con la API real de Anthropic (`ANTHROPIC_API_KEY=... kling db ask-web t1`)
está pendiente de una clave: solo hay cubierta la petición con tests unitarios.

## Proveedores

- **anthropic**: la API Messages, con `ANTHROPIC_API_KEY`.
- **opencode**: el CLI [opencode](https://opencode.ai) (se busca en el `PATH` y en
  `~/.opencode/bin`) hablando con MiniMax u otro modelo que tengas configurado en él
  (`-model proveedor/modelo`; por defecto `minimax-coding-plan/MiniMax-M2.7`). Se ejecuta
  como `opencode run --pure -m <modelo> --format json`:
  - `--pure`, sin plugins; **nunca** `--auto`.
  - En un directorio temporal nuevo y vacío (0700) que se borra al acabar.
  - El prompt (instrucciones, esquema y pregunta) va por argumento hasta 100 KiB (lo
    puede ver con `ps` cualquier usuario del equipo; no lleva secretos) y, si es mayor,
    en un fichero adjunto de ese directorio.
  - Solo se aceptan los eventos `text` y `step_start`/`step_finish`. Si opencode intenta
    usar una herramienta, pide un permiso o falla, `ask` aborta y mata el proceso.
  - El entorno del proceso es **el tuyo**, sin filtrar: opencode necesita su configuración
    y sus credenciales. opencode guarda además la sesión (el prompt y la respuesta) en su
    propio almacén local, como en cualquier otro uso suyo.
  - De la respuesta se extrae la sentencia (bloque ` ```sql `, o el texto entero); lo
    dudoso (varias sentencias, texto suelto) lo rechaza `sqlguard`, igual que con
    Anthropic. Nada de esto relaja los pasos siguientes.
- **`KLING_DB_ASK_FAKE=<fichero>`, solo para pruebas** (e2e sin red): la respuesta del
  "modelo" es el contenido del fichero (hasta 64 KiB) y manda sobre `-provider`. No salta
  ningún control: la SQL pasa por `sqlguard`, el rol de solo lectura y la transacción
  READ ONLY como cualquier otra.

## Qué sale de tu máquina y qué no

| sale hacia el proveedor (la API de Anthropic, o MiniMax a través de opencode) | NO sale nunca |
|---|---|
| la pregunta | la contraseña de la copia (ni al modelo, ni a argv, ni a la salida) |
| el esquema que el rol de solo lectura puede ver: esquemas, tablas y vistas, columnas con su tipo y `NOT NULL`, claves primarias, únicas y foráneas, y los **comentarios** (`COMMENT ON`) de tablas y columnas | los datos de las tablas (salvo con `-explain -send-data`) |
| con `-explain -send-data`: la SQL ejecutada, sus columnas y **hasta 50 filas** del resultado | valores por defecto de columnas y restricciones `CHECK` (pueden llevar literales) |
| | estadísticas, tamaños, roles, configuración |
| | la clave de la API en ningún sitio salvo la cabecera `x-api-key` de la petición |

- Los comentarios del esquema **sí** salen: si alguno lleva algo que no deba salir,
  quítalo de la copia (`COMMENT ON ... IS NULL`) o usa `-role` con un rol que no vea esa
  tabla (solo se manda lo que el rol puede consultar).
- Sin `-send-data`, `-explain` se rechaza antes de hacer nada. Con él, `ask` avisa por
  stderr de cuántas filas manda y a quién.
- La petición va a `https://api.anthropic.com/v1/messages` con la biblioteca estándar de
  Go, sin seguir redirecciones (la cabecera de la clave no saldría hacia otro sitio).
  La clave se lee de `ANTHROPIC_API_KEY`; sin ella ni opencode, `ask` falla antes de tocar
  la copia.
- Con opencode sale lo mismo (esquema y pregunta; filas solo con `-explain -send-data`),
  pero hacia el servicio que opencode tenga para ese modelo (para MiniMax, el suyo), con
  las credenciales de opencode: `kling` no las ve ni las toca.

## Cómo se ejecuta la consulta

El modelo **no ejecuta nada ni recibe credenciales**: devuelve texto. Todo lo demás pasa
en el host y en la copia, en este orden:

1. **La copia**, como `connect`: tuya, en marcha, `ready` y con la contraseña de su id
   en este host.
2. **El rol de solo lectura.** Sin `-role`, `ask` crea (si falta) `kling_db_ro`: `LOGIN`,
   sin ningún atributo de administración, miembro de `pg_read_all_data` y con
   `default_transaction_read_only = on`. Con `-role`, el rol tiene que existir; `ask` no
   crea ni cambia nada de él. En los dos casos se **comprueba** antes de usarlo, y se
   rechaza si es superusuario, tiene `CREATEROLE`, `CREATEDB`, `REPLICATION` o
   `BYPASSRLS`, pertenece a **cualquier** rol que no sea `pg_read_all_data` (directa o
   indirectamente), es dueño de la base o de alguna relación, o puede escribir en
   alguna. Nunca vale el rol de la aplicación (`app`) ni `postgres`. La pertenencia se
   mira con `pg_has_role(..., 'MEMBER')` y en `pg_auth_members`, no con `'USAGE'`: en
   PostgreSQL 16 un `GRANT app TO lector WITH INHERIT FALSE` no hereda privilegios
   (`USAGE` daría falso) pero deja hacer `SET ROLE app`.
3. **Entrar como ese rol.** El `pg_hba` de la plantilla solo deja entrar a `postgres` por
   el socket y a `app` por red. `ask` añade (una vez, idempotente) una línea
   `local all <rol> peer map=kling_db_ask` a `pg_hba.conf` y `kling_db_ask postgres <rol>`
   a `pg_ident.conf`, y recarga la configuración. Así el usuario del sistema `postgres`
   del invitado entra **como el rol** por el socket, sin contraseña: la sesión es de ese
   rol desde la autenticación. No hay `SET ROLE` que la consulta pudiera deshacer (con
   `set_config('role', ...)`, un `SET ROLE` desde una sesión de superusuario no es una
   frontera).
4. **El esquema** se lee con ese rol, en una transacción de solo lectura.
5. **El modelo** recibe esquema y pregunta y devuelve una sentencia. Se quita un `;`
   final si lo trae.
6. **Primera barrera: el validador del host** (`ext/db/internal/sqlguard`). Acepta solo
   una sentencia que empiece por `SELECT` o `WITH` tras quitar comentarios (`--` y
   `/* */` anidados, como Postgres), y rechaza: `;`, barras invertidas (metacomandos de
   psql como `\!`, y `E'...'`), `$` (dollar quoting y parámetros), escapes `U&`/`UESCAPE`,
   cadenas, identificadores o comentarios sin cerrar, paréntesis desequilibrados,
   caracteres de control, las palabras `INSERT`, `UPDATE`, `DELETE`, `MERGE`,
   `TRUNCATE`, `INTO`, DDL, `GRANT`/`REVOKE`, `COPY`, `DO`, `CALL`, `SET`, `RESET`,
   transacciones, `LISTEN`/`NOTIFY`, `LOCK`, `VACUUM`, `ANALYZE`, `EXPLAIN`,
   `FOR UPDATE/SHARE`..., y funciones (también entre comillas o con esquema) como
   `pg_sleep*`, `dblink*`, `lo_*`, `pg_read_file`, `pg_read_binary_file`, `pg_ls_*`,
   `pg_stat_file`, `set_config`, `pg_terminate_backend`, `pg_advisory_*`, `nextval`,
   `setval`, `pg_notify`, `pg_reload_conf` o `query_to_xml` (ejecuta SQL guardado en
   una cadena). Es deliberadamente estrecho: ante la duda, rechaza.
7. **Se enseña la SQL** (por stderr) y se pide confirmación, salvo `-yes`. Sin terminal y
   sin `-yes`, la respuesta vacía es "no".
8. **La garantía real: cómo se ejecuta.** Por `kling exec -i`, con la sentencia por stdin
   (nunca en argv) al `psql` que entra **como el rol de solo lectura**, con
   `PGOPTIONS='-c default_transaction_read_only=on'`, y dentro de:

   ```sql
   BEGIN TRANSACTION READ ONLY;
   SET LOCAL statement_timeout = <ms>;
   SET LOCAL lock_timeout = <ms>;
   SET LOCAL idle_in_transaction_session_timeout = <ms>;
   DO $k$ ... RAISE si session_user/current_user no son el rol,
             si es superusuario o si la transacción no es de solo lectura ... $k$;
   SELECT * FROM (
   <la sentencia>
   ) q LIMIT <n>;
   ROLLBACK;
   ```

   Justo antes de este paso se vuelve a rechazar cualquier barra invertida, aunque el
   validador hubiera fallado: un metacomando de psql es lo único que saldría de Postgres.
   Si el validador dejara pasar algo peor, sigue ejecutándose como un rol sin permisos de
   escritura, en solo lectura y con plazo. Los tests
   (`ext/db/cmd/kling-db/cmd_ask_test.go`) anulan el validador y comprueban exactamente
   eso sobre los argumentos y el stdin que llegan a `kling exec`.

9. **Resultado**: una tabla (celdas en una línea, sin caracteres de control, cortadas a
   60 caracteres) o `-json`. Si hay tantas filas como `-limit`, lo dice.

## Límites

- `kling_db_ro` lee **todo** lo que no proteja RLS (`pg_read_all_data`). Para limitar lo
  que se ve (y lo que sale en el esquema), crea un rol propio con `SELECT` solo sobre lo
  que haga falta, sin atributos, y pásalo con `-role` (`ask` le da acceso por el socket).
- Las funciones `SECURITY DEFINER` de la base se ejecutan con los permisos de su dueño;
  la transacción READ ONLY impide que escriban, pero pueden leer lo que su dueño lea.
  El validador no conoce las funciones propias de cada base.
- `kling_db_ro`, la línea de `pg_hba.conf` y la de `pg_ident.conf` se quedan en la copia
  y pasan a sus `fork`, `undo` y golden: a propósito, porque no dan nada nuevo. El rol no
  tiene contraseña ni línea de red; solo lo puede usar el usuario del sistema `postgres`
  del invitado, que ya es superusuario por peer. Para que siga así, `up`, `fork` y `undo`
  le quitan la contraseña si alguien le hubiera puesto una (`PASSWORD NULL`). Los roles
  de `kling db role` (comentario `kling-db:ro`), que sí tienen clave y línea de red, en
  cambio **no** pasan: se borran al preparar la copia nueva (ver [db.md](db.md)).
- Si una línea anterior de `pg_hba.conf` rechaza al rol por el socket (no pasa con las
  plantillas de `scripts/db-golden.sh`), `ask` falla con ese mensaje.
- El esquema enviado se corta en 500 relaciones y 256 KiB.
- `ask -role agent` con un rol de `kling db role` funciona (le da acceso por el socket),
  pero ese rol no sobrevive a un `fork` o `undo`: en la copia nueva hay que crearlo otra
  vez.
