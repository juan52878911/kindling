# MySQL y MariaDB: el proxy de credenciales y `kling db`

Dos piezas, las mismas que para Postgres ([postgres.md](postgres.md), [db.md](db.md)):

- **El proxy de credenciales** (`kling machine credential -type mysql`,
  `pkg/credproxy/mysql.go`): un agente usa tu MySQL o tu MariaDB **sin ver nunca la
  contraseña**. Recibe un marcador (`kling-cred-…`) como contraseña; el proxy del host
  entra en el servidor con la clave real.
- **Copias desechables** (`kling db up` de una plantilla MariaDB): una base por microVM,
  con su propia contraseña que solo conoce el host.

El modelo de seguridad completo está en [SECURITY.md §7 y §17](../SECURITY.md).

Alcance de esta versión (#64), decidido por seguridad antes que por cobertura:

| hecho | pendiente |
|---|---|
| proxy `-type mysql`: saludo propio, marcador por `mysql_native_password` o la ruta rápida de `caching_sha2_password`, TLS verify-full al servidor, clave real por `caching_sha2_password` (completa, dentro del TLS) o `mysql_native_password` | `KILL QUERY` mapeado (exige reescribir SQL); RSA para `caching_sha2_password` sin TLS |
| goldens MariaDB (Alpine) con `db-golden-mysql.sh` / `db-golden.sh -engine mysql` | goldens MySQL 8 de Oracle (Alpine no lo empaqueta) y hash `caching_sha2_password` en el host |
| `kling db up/fork/connect/rotate/reset/rm/doctor/audit/branch` sobre copias MySQL | `attach/detach`, `role`, `rehearse`, `snapshot/undo`, `tenant-check`, `ask`, `clone` y `doctor -url` para MySQL: se rechazan con un error claro |

## El proxy de credenciales

```
 microVM (egress allowlist)            host (daemon o kling-vz)                 servidor
 ─────────────────────────             ────────────────────────                 ────────
 mysql -h db.internal -u app  ─claro─►  proxy de MySQL   ──TLS verify-full──►   MySQL / MariaDB
   password=kling-cred-…                · saluda él (nonce propio)
                                        · elige la credencial por el marcador
                                        · exige su usuario (y su base de arranque)
                                        · entra con la clave real
```

```sh
kling run -name a1 -image node -egress allowlist -allow db.example.com
kling machine credential a1 -type mysql -domain db.example.com -user app \
  -database appdb -env MYSQL_PWD -f clave.txt
# dentro: mysql -h db.example.com -u app --ssl-mode=DISABLED appdb   (MYSQL_PWD = el marcador)
```

Las banderas son las de Postgres (`-port`, 3306 por defecto; `-user`, `-database` o
`-any-database`, `-ca-file`, `-upstream`, `-upstream-tls`, `-tls-server-name`), y valen
igual en `kling template credential`.

### Cómo va, paso a paso

1. **El saludo lo manda el proxy.** En MySQL habla primero el servidor, así que el
   proxy manda su propio saludo: versión `8.0.40-kindling-credproxy`, id de conexión
   `0`, un nonce aleatorio y `mysql_native_password` como método. **No ofrece TLS** en
   este tramo (que no sale de la máquina): un cliente con `--ssl-mode=REQUIRED` o más
   falla en su lado; usa `DISABLED` o `PREFERRED`. Tampoco ofrece compresión, `LOCAL
   INFILE` ni atributos de conexión.
2. **La prueba del marcador.** El cliente contesta con el scramble de
   `mysql_native_password` o el de `caching_sha2_password` (la ruta rápida) calculado
   sobre el marcador; cualquier otro método recibe un `AuthSwitchRequest` a
   `mysql_native_password` con el mismo nonce. El proxy calcula lo que daría el marcador
   de **cada** credencial MySQL y lo compara en tiempo constante: así elige la
   credencial. Después exige el usuario de la credencial y, si fija base, esa o ninguna.
3. **Hacia el servidor**, como en Postgres: sin `-upstream`, solo a IPv4 públicas; con
   él, a la dirección fijada (loopback y privadas sí, metadatos y la red de kindling
   nunca). Lee el saludo del servidor con su longitud exacta y sin buffer, exige
   `CLIENT_SSL`, manda el `SSLRequest` y negocia TLS 1.2+ verificado contra el dominio
   (o `-tls-server-name`) con las raíces del sistema más `-ca-file`. Lo que un
   intermediario meta detrás del saludo acaba dentro del handshake y lo rompe.
4. **La clave real**: `caching_sha2_password` (scramble; si el servidor pide la
   autenticación completa, la clave en claro **dentro** del TLS verificado, como hace
   cualquier cliente) o `mysql_native_password`; `mysql_clear_password` tras un cambio
   de método, solo dentro del TLS. Nada más: ni `sha256_password`, ni ed25519, ni
   GSSAPI, ni PAM.
5. **El invitado recibe un OK** solo cuando lo dio el servidor (el mismo OK). Un error
   del servidor antes de eso no se reenvía: el invitado recibe uno propio (1045 o 2003,
   con el código del servidor como mucho) y el log del host lleva solo ese código.
6. Después, bytes tal cual. Cada conexión deja una línea en `kling machine audit`
   (`MYSQL`, `kind: mysql`), sin la clave, el marcador ni el SQL.

### Capacidades

Tras autenticar, invitado y servidor tienen que hablar el mismo dialecto (el proxy
empalma bytes). El proxy exige que el servidor tenga las capacidades que cambian el
formato de lo que viaja (`DEPRECATE_EOF`, `SESSION_TRACK`, `MULTI_RESULTS`...) si el
invitado las eligió; si no, error propio (`2003`, motivo `capabilities`) **antes** de
mandar la clave. MySQL 5.7+ y MariaDB 10.2+ las tienen todas.

### `-upstream-tls disable`

Solo con `-upstream`. Se admiten `mysql_native_password` y la ruta rápida de
`caching_sha2_password`, que **no mandan la clave**; la autenticación completa y
`mysql_clear_password` se rechazan (no se hace el intercambio RSA con la clave pública
que manda el propio servidor: no lo autentica). Es **más débil que en Postgres**: en
MySQL el servidor no prueba que conoce la clave (no hay firma como la de SCRAM). Quien
conteste en la dirección fijada se queda con las consultas y con un scramble, que
permite atacar la clave por diccionario (inútil contra una aleatoria larga). Para un
Docker en el loopback vale; hacia la LAN, la CLI lo avisa: mejor TLS con `-ca-file`.

Con MySQL 8 y `caching_sha2_password`, sin TLS, la primera conexión tras arrancar el
servidor pide la autenticación completa y el proxy la rechaza (el servidor aún no tiene
la clave en su caché). O TLS, o `mysql_native_password` para ese usuario.

### KILL QUERY no se mapea

En MySQL cancelar es abrir otra conexión y mandar `KILL QUERY <id>` con el id que dio el
saludo. Ese id es del proxy (`0`: el saludo sale antes de saber a qué servidor se va) y
mapearlo exigiría reescribir SQL en el flujo, que el proxy no mira. Un cliente que
cancela así (Connector/J con `setQueryTimeout`, por ejemplo) recibe `Unknown thread id:
0` y la consulta sigue hasta el final o hasta su `max_execution_time`. El id real sale
de `SELECT CONNECTION_ID()`, y un `KILL QUERY` con él, por el proxy y como el mismo
usuario, funciona (lo comprueba la prueba `mylab`).

### Lo que no resuelve

- **La base no es una frontera.** Con `-database appdb` el invitado arranca en `appdb`,
  pero puede hacer `USE otra` o leer `otra.tabla` si el usuario tiene `GRANT` allí. Lo
  que acota es el usuario: `GRANT ... ON appdb.*`, sin `FILE`, `SUPER`, `PROCESS` ni
  `GRANT OPTION`. La CLI lo recuerda.
- El usuario puede cambiarse la contraseña (`ALTER USER`/`SET PASSWORD` sobre sí mismo):
  no la leería, pero la clave del proxy dejaría de valer.
- La versión del saludo es la del proxy; `SELECT VERSION()` da la real.

### Cómo llega el invitado

Todo el TCP de la IP del proxy que no es 53, 80 ni 443 llega al proxy de bases de datos.
Con solo credenciales MySQL, todo es MySQL; con solo Postgres, todo Postgres. **Con las
dos en la misma máquina, MySQL es el 3306** (una credencial MySQL en otro puerto o una
Postgres en el 3306 se rechazan): en Linux el DNAT del netns no deja ver al host el
puerto original, así que el 3306 tiene su propio DNAT a `n.HostIP:5382`; en macOS
`kling-vz` ve el puerto tal cual.

### Prueba contra un servidor real

`pkg/credproxy/mysql_lab_test.go` (etiqueta `mylab`): autenticación con la clave real y
una consulta (native y caching_sha2), un marcador equivocado (sin conectar al servidor)
y `KILL QUERY` con el id del saludo (no cancela) y con el real (sí). La cabecera trae
las variables y una receta con Docker.

## Copias desechables: `kling db` con MariaDB

```sh
kling db golden -script scripts/db-golden.sh image -engine mysql      # la imagen, una vez
kling db golden -script scripts/db-golden.sh build -engine mysql -seed-mb 20 my
kling db up my -name m1                 # una copia lista, con su propia contraseña
kling db connect m1 -mysql              # el cliente del host (mariadb o mysql)
kling db connect m1 -dsn                # mysql://app:…@IP:3306/appdb
kling db fork m1 -n 4 · kling db rotate m1 · kling db doctor m1 · kling db audit m1 · kling db rm m1
```

### La plantilla (`scripts/db-golden-mysql.sh`)

- **Receta** `scripts/recipes/mariadb.recipe.json`: Alpine + `mariadb`, `mariadb-client`
  y `tzdata`. MySQL 8 de Oracle no está en Alpine; una plantilla con MySQL 8 instalado
  (`-from`) sigue el mismo camino, pero no se ha probado en esta versión.
- **El datadir en el overlay** de la máquina (`/var/lib/mysql`), no en un volumen: se
  congela y se ramifica con ella, como el cluster de Postgres.
- **Configuración** (`/etc/my.cnf.d/zz-kling-db.cnf`): escucha en la IP del invitado,
  `skip-name-resolve`, `local-infile = 0`, `secure-file-priv` en un directorio vacío,
  sin log general ni de consultas lentas (ni SQL ni datos en disco) y **server_audit
  solo con `CONNECT`** (quién, desde dónde y a qué base; `FORCE_PLUS_PERMANENT`) para
  `kling db audit`. Si la imagen no trae `server_audit.so`, el script avisa y `audit`
  solo verá los eventos del daemon.
- **Cuentas**: `root@localhost` solo por `unix_socket` (el root del sistema del
  invitado; así entra `kling db`), fuera las anónimas y cualquier `root`/`mysql` que no
  sea `localhost`, fuera la base `test`. El usuario de la aplicación es `app@'%'` con
  `GRANT ALL ON appdb.*`: nada global.
- **La contraseña de la plantilla** se genera en el host (192 bits) y se guarda solo
  ahí (`$STATE/<nombre>/password`, 0600). Al invitado va **su hash**, calculado en el
  host con `openssl` (o `python3`): sin ninguno de los dos no se construye. No hay
  camino en claro.
- **Migraciones y seed como root** (en MySQL no hay `SET ROLE` a un usuario): lo que
  creen con `DEFINER` (vistas, rutinas, disparadores, eventos) correrá como root.
  `doctor` lo avisa (`MY020`); créalos con `DEFINER = 'app'@'%'` o `SQL SECURITY
  INVOKER`. `-seed-mb N` usa `seq_1_to_N` (el motor SEQUENCE de MariaDB).
- **Etiquetas**: la plantilla lleva `kling.db.engine=mysql` (y rol y base), que heredan
  las copias; `conn.env` dice `ENGINE=mysql`, `DBUSER`, `DBNAME`.

### La rotación en cada copia

`kling db up` y `fork` generan la clave en el host, la escriben en
`copies/<id>/password` (0600) y mandan al invitado, por stdin y al cliente de root por el
socket, `ALTER USER 'app'@'%' IDENTIFIED WITH mysql_native_password AS '<hash>'`; en la
misma sesión comprueban que `mysql.user` guarda ese hash con ese plugin. La copia no se
marca `ready` hasta entonces, y si algo falla se destruye.

**Por qué `mysql_native_password`**: es el único método que MariaDB y MySQL 8.0 entienden
igual y cuyo formato guardado (`*` + `HEX(SHA1(SHA1(clave)))`) se calcula en el host sin
más. Su debilidad es esa (SHA-1 rápido y sin sal: un hash filtrado se ataca por
diccionario), y con una clave de 192 bits aleatorios no hay diccionario que valga.
`caching_sha2_password` (el de MySQL 8.4+) guarda un SHA-256-crypt con sal y 5000
rondas: calcularlo aquí es posible, pero no se ha validado contra un servidor real y
queda pendiente, igual que MySQL 9 (que ya no trae `mysql_native_password`).

### doctor (reglas MY)

| regla | qué |
|---|---|
| MY001 | cuenta de red con privilegios de servidor (`SUPER`, `FILE`, `PROCESS`, `SHUTDOWN`, `RELOAD`, `CREATE USER`, `GRANT OPTION`, `*ADMIN`); las locales (root) son INFO |
| MY002 | cuenta de red con privilegios sobre todas las bases (`ON *.*`) |
| MY003, MY004 | cuentas anónimas; cuentas con un método de contraseña y sin contraseña (las bloqueadas y los roles no cuentan) |
| MY010, MY011 | `local_infile` activo; `secure_file_priv` vacío (cualquier directorio) |
| MY012 | copias: el registro de conexiones (`server_audit`) no está activo |
| MY020 | vistas, rutinas, disparadores o eventos que corren como una cuenta con privilegios de servidor |
| MY030 | INFO: el usuario de la app usa `mysql_native_password` |
| MY050-MY054 | copias: reloj, conexiones abiertas (INFO), la clave sigue siendo la del dorado o no es un hash que se pueda comprobar, `kling.db.state` no es `ready`, el fichero de la clave en el host falta, está abierto o no casa |

`doctor -url mysql://...` no existe todavía: el doctor tendría que hablar el protocolo
(y TLS) desde el host.

### audit

`kling db audit <copia>` lee `/var/log/mysql/audit.log` (server_audit) y lo une a los
eventos del daemon: `connect`, `disconnect` y `auth-failed` con usuario, base y cliente.
Usuario y base los elige el cliente y pueden llevar comas: una línea que no tiene los
diez campos se enseña con `?` en vez de interpretarse a medias.

### Lo que se rechaza en copias MySQL

`attach`/`detach` (el modelo A necesita, del lado del proxy, que la copia pruebe que
conoce la clave, y en MySQL no puede), `role`, `rehearse`, `snapshot`/`undo`,
`tenant-check`, `ask`, `diff`, `env` y `clone` hablan SQL de Postgres o usan su
catálogo: con una copia MySQL dan `kling db <cmd> supports postgres copies only in this
version` (`diff` y `env` lo dicen desde #65; antes fallaban más tarde y sin explicarlo).
Redis y SQLite, en [db-engines.md](db-engines.md).
