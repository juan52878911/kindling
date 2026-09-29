# Conectar tu base de datos (Postgres)

Un agente dentro de una microVM puede usar tu PostgreSQL **sin ver nunca la
contraseña**. El invitado recibe un marcador (`kling-cred-…`) como contraseña; el
proxy de credenciales del host lo cambia por la clave real al autenticarse contra el
servidor. Este documento es la guía práctica: cómo funciona, recetas para los casos
habituales (un Docker en el mismo host, una base de datos en la LAN o la VPC, Neon,
Supabase y RDS), cómo configurar el cliente y qué no resuelve. El modelo de seguridad
completo está en [SECURITY.md §7](../SECURITY.md); la API, en
[api.md](api.md#credenciales-de-postgres). Para MySQL y MariaDB (`-type mysql`), el
mismo modelo con sus diferencias: [mysql.md](mysql.md).

## Cómo funciona

```
 microVM (egress allowlist)          host (daemon o kling-vz)              servidor
 ─────────────────────────           ────────────────────────              ────────
 psql host=db.internal  ──claro──►   proxy de Postgres  ──TLS verify-full──►  Postgres
   password=kling-cred-…             · elige la credencial por el marcador     (SCRAM)
                                     · exige su rol y su base
                                     · entra con la clave real
```

1. La máquina corre con `-egress allowlist`. Su resolver contesta el dominio de la
   credencial (`-domain`) con la IP del proxy, sea cual sea el puerto al que conecte.
2. El tramo del invitado va **en claro** y no sale de la máquina (el veth de su netns
   en Linux, la pila gVisor de su `kling-vz` en macOS). Por él solo viaja el marcador.
3. El proxy pide la contraseña, la compara con los marcadores, exige que el rol (y la
   base, si la credencial la fija) sean los de la credencial, y entra en el servidor.
4. **A dónde marca**: sin `-upstream`, a `dominio:puerto`, solo si el nombre resuelve a
   una IPv4 pública. Con `-upstream host:puerto`, a esa dirección, que fija el operador
   (loopback y privadas permitidas; ver [Límites](#límites)).
5. **Cómo**: por defecto TLS verify-full (TLS 1.2+, raíces del sistema más `-ca-file`,
   nombre `-tls-server-name` o, si no, el dominio) y SCRAM-SHA-256 (`-PLUS` si el
   servidor lo ofrece). Con `-upstream-tls disable`, sin TLS y **solo** SCRAM-SHA-256.
6. Tras la autenticación el flujo pasa tal cual. Cada conexión deja una línea en
   `kling machine audit` (`kind: postgres`, con `upstream` si lo hay), sin la clave, el
   marcador ni el SQL.

La contraseña se lee de `-f` o de stdin, nunca de la línea de comandos. Las mismas
banderas valen para `kling template credential` (cada instancia de la plantilla recibe
su propio marcador).

## Banderas

| Bandera | Qué hace |
|---|---|
| `-type postgres` | credencial de base de datos |
| `-domain N` | el nombre al que conecta el invitado. Sin `-upstream`, también el servidor al que se sale |
| `-user R` | el rol; el invitado tiene que conectar con él |
| `-database B` | **obligatoria** salvo `-any-database`: la única base permitida |
| `-any-database` | en lugar de `-database`: deja entrar en cualquier base a la que el rol tenga `CONNECT`. El CLI avisa por stderr. Son excluyentes |
| `-port P` | puerto del servidor sin `-upstream` (5432). El invitado puede usar cualquiera |
| `-ca-file ca.pem` | CA que se añade a las raíces del sistema |
| `-upstream H:P` | a dónde marca el proxy en vez de `dominio:puerto` (IP o nombre, `[::1]:5432` para IPv6) |
| `-upstream-tls verify-full\|disable` | `verify-full` por defecto; `disable` solo con `-upstream` |
| `-tls-server-name N` | nombre contra el que se verifica el certificado (por defecto `-domain`) |
| `-env PGPASSWORD` | la variable que recibe el marcador |

## Recetas

### Docker en el mismo host, sin TLS

El caso de desarrollo: un `postgres` en Docker publicado solo en el loopback.

```sh
docker run -d --name appdb -p 127.0.0.1:5432:5432 \
  -e POSTGRES_USER=app -e POSTGRES_PASSWORD='la-clave' -e POSTGRES_DB=appdb postgres:17

kling run -name agente -image toolchain -egress allowlist -allow-exec
printf '%s' 'la-clave' | kling machine credential agente -type postgres \
  -domain db.internal -user app -database appdb \
  -upstream 127.0.0.1:5432 -upstream-tls disable -env PGPASSWORD
# dentro: psql "host=db.internal user=app dbname=appdb sslmode=disable"
```

- `db.internal` es solo el nombre que usa el invitado: cualquier nombre exacto con un
  dominio de primer nivel alfabético sirve (`pg.kindling.test`, `db.internal`…).
- **Dónde está `127.0.0.1`**: en Linux el proxy es una goroutine del daemon y marca
  desde el netns del host (no desde el de la máquina), así que es el loopback del host
  (en un CT de Proxmox, el del CT). En macOS lo marca `kling-vz` con la pila de red del
  Mac, no con la gVisor del invitado: es el loopback del Mac, donde Docker Desktop
  publica los puertos.
- **Puertos reservados `29000-29999` del loopback**: en macOS cada `kling-vz` expone
  los puertos de su invitado en `127.0.0.1` y solo dentro de ese rango. Un `-upstream`
  del loopback (`127.0.0.0/8`, `::1` o `localhost`) con un puerto del rango se rechaza
  al darlo y, otra vez, al marcar: así no puede llegar al invitado de otra máquina. La
  regla es la misma en Linux. Publica la base de datos en otro puerto.
- En Linux también vale la IP del contenedor en su red (`-upstream 172.17.0.2:5432`):
  la red de Docker no es de kindling. Publicar en `127.0.0.1` es más estable (la IP
  del contenedor cambia al recrearlo).
- `-upstream-tls disable` exige que el servidor pida **SCRAM-SHA-256**. La imagen
  oficial lo hace desde Postgres 14 cuando se da `POSTGRES_PASSWORD`
  (`password_encryption = scram-sha-256` y `scram-sha-256` en `pg_hba.conf`). Un rol con
  contraseña md5, `trust` o `password` en `pg_hba.conf` se rechaza con
  `upstream without TLS requires SCRAM-SHA-256` en el log del daemon y 28P01 en el
  invitado. Sin TLS la contraseña no cruza el socket (SCRAM prueba que se conoce sin
  mandarla), pero las consultas sí van en claro: en el loopback no salen del host.

Con un Postgres de Docker que sí tenga TLS (un certificado propio), quita
`-upstream-tls disable` y añade `-ca-file` y `-tls-server-name` como en la receta
siguiente.

### Base de datos en la LAN o en la VPC, con TLS

Un servidor con IP privada, con su certificado emitido por una CA propia a nombre de
`pg.lan.example.com`:

```sh
printf '%s' "$PGPASS" | kling machine credential agente -type postgres \
  -domain pg.lan.example.com -user app -database appdb \
  -upstream 10.0.3.25:5432 -ca-file corp-ca.pem -env PGPASSWORD
```

- El certificado se verifica contra `-domain`. Si lleva otro nombre (el invitado usa
  `db.internal` pero el certificado es de `pg.lan.example.com`, o solo lleva la IP),
  usa `-tls-server-name pg.lan.example.com` (o `-tls-server-name 10.0.3.25`).
- En Linux, `-upstream` admite un nombre: `-upstream pg.lan.example.com:5432` se
  resuelve **en el host**, con su resolver del sistema (`/etc/hosts`, el DNS de la
  VPC…), al abrir cada conexión. Si alguna de sus IPs cae en un rango prohibido, no se
  marca ninguna. **En macOS el nombre lo resuelve el daemon**: `kling-vz` corre
  confinado (`kling-vz.sb`) y desde ahí el resolver del Mac no contesta, así que el
  daemon resuelve el nombre con el resolver del Mac al entregar la credencial (al
  ponerla y en cada arranque o descongelación), le aplica las mismas comprobaciones
  que al marcar y le pasa a `kling-vz` la primera IP. Dos diferencias con Linux: un
  cambio de DNS no se sigue hasta la siguiente entrega (`kling machine credential`
  otra vez, o un freeze/thaw), y no hay reintento con las demás IPs del nombre. El
  certificado se sigue verificando contra `-domain` o `-tls-server-name`, nunca
  contra la IP.
- Un RDS privado en la VPC (daemon Linux en la VPC): `-upstream
  mydb.xxxx.eu-west-1.rds.amazonaws.com:5432` con el mismo nombre en `-domain` y
  `-ca-file global-bundle.pem` (ver RDS abajo).

Sin TLS hacia la LAN (`-upstream-tls disable` con una IP no loopback) la CLI avisa:

```
warning: -upstream-tls disable: traffic to 10.0.3.25:5432, including query data, is unencrypted (the password is not: SCRAM-SHA-256 only)
```

Quien esté en esa red no se lleva la clave, pero puede leer y cambiar lo que pase
después de autenticar. Para la LAN, mejor TLS.

### Neon, Supabase y RDS públicos (verify-full)

Sin `-upstream`: el proxy resuelve el dominio, exige una IPv4 pública y verifica el
certificado contra ese nombre.

**Neon** (certificado de una CA pública; el nombre del endpoint va en el SNI, que el
proxy manda):

```sh
printf '%s' "$NEON_PASSWORD" | kling machine credential agente -type postgres \
  -domain ep-cool-name-123456.eu-central-1.aws.neon.tech -user app -database neondb \
  -env PGPASSWORD
```

**Supabase**: la conexión directa (`db.<ref>.supabase.co`) es solo IPv6 salvo que el
proyecto tenga el complemento IPv4, y el proxy sin `-upstream` solo sale por IPv4. Usa
el pooler (Supavisor), cuyo rol es `postgres.<ref>`, con la CA que el panel ofrece para
descargar (Database settings → SSL configuration):

```sh
printf '%s' "$SUPABASE_DB_PASSWORD" | kling machine credential agente -type postgres \
  -domain aws-0-eu-central-1.pooler.supabase.com -port 5432 -user postgres.abcdefghijklmnop \
  -database postgres -ca-file prod-ca-2021.crt -env PGPASSWORD
```

**RDS / Aurora** con acceso público, con el paquete de CAs de AWS
(`https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem`):

```sh
printf '%s' "$RDS_PASSWORD" | kling machine credential agente -type postgres \
  -domain mydb.abcdefghijkl.eu-west-1.rds.amazonaws.com -user app -database appdb \
  -ca-file global-bundle.pem -env PGPASSWORD
```

El rol tiene que tener una contraseña SCRAM (`password_encryption = scram-sha-256`,
el valor por defecto desde Postgres 14; en RDS, el grupo de parámetros). md5 no se
admite. La autenticación IAM de RDS (tokens que caducan cada 15 minutos) tampoco: la
credencial es una contraseña fija.

**Probado contra RDS real** (2026-09-29, `us-east-1`, RDS Postgres 16.13 `db.t4g.micro`
con acceso público restringido a una IP): desde una microVM del laboratorio, con solo el
marcador, el proxy entra con **SCRAM-SHA-256-PLUS** (channel binding sobre TLS verificado
con el paquete regional de CAs de RDS), consulta y cancela `pg_sleep`; sin `-ca-file` el
proxy se niega (`upstream_tls`: la CA de RDS no está entre las raíces del sistema), así
que la contraseña nunca sale; un marcador inventado se rechaza sin tocar RDS. Neon,
Supabase y pgbouncer siguen sin probar de verdad (#63).

## El cliente dentro de la microVM

| Ajuste | Valor |
|---|---|
| host | el `-domain` de la credencial |
| puerto | cualquiera (5432 por costumbre): todos llegan al proxy |
| usuario | el `-user` de la credencial |
| base | la de `-database` (con `-any-database`, cualquiera) |
| contraseña | `$PGPASSWORD` (el marcador) |
| `sslmode` | `disable` o `prefer`. `require`, `verify-ca` y `verify-full` **no conectan**: el tramo del invitado no tiene TLS (el proxy contesta `N`) |
| `channel_binding` | `disable` o `prefer`, **nunca** `require` (no hay TLS al que atar) |
| `gssencmode` | cualquiera salvo `require` |

Ejemplos:

```sh
psql "host=db.internal user=app dbname=appdb sslmode=disable"
export DATABASE_URL="postgres://app:${PGPASSWORD}@db.internal:5432/appdb?sslmode=disable"
```

En node-postgres, `ssl: false`; en psycopg, `sslmode="disable"`; en JDBC,
`sslmode=disable`. Si el marcador acaba en una URL, no hace falta escaparlo: es
`kling-cred-` y hexadecimal.

## Límites

- **Solo el operador fija el upstream**, desde el host (CLI o API del daemon). El
  invitado no lo ve ni lo elige.
- **Destinos prohibidos, incluso fijados**: `169.254.0.0/16` (metadatos del cloud),
  `0.0.0.0/8`, multicast (`224.0.0.0/4`, `ff00::/8`), `240.0.0.0/4`, `::`, `fe80::/10`,
  `fd00:ec2::254`, y la red de kindling: `172.16.0.0/30` (enlace del invitado) y
  `172.30.0.0/16` (veth del host en Linux). Una IPv4 escrita como IPv6
  (`::ffff:169.254.169.254`) cuenta como la IPv4.
- `-upstream-tls disable` solo con `-upstream`, sin `-ca-file` ni `-tls-server-name`, y
  solo con SCRAM-SHA-256 (ni `-PLUS`, ni contraseña en claro, ni md5, ni trust).
- Sin `-upstream`, el proxy solo sale a IPv4 públicas (un DNS que devuelva una IP
  privada no lleva la clave a la LAN). Con `-upstream`, IPv4 e IPv6.
- Un upstream por credencial: sin varios hosts, sin `target_session_attrs`, sin
  conmutación por error.
- Autenticación hacia el servidor: SCRAM-SHA-256 (con `-PLUS` sobre TLS) o, solo
  dentro de TLS verificado, contraseña en claro (la auditoría anota `auth: password`, no
  `scram-sha-256`, y el log del host avisa una vez por credencial: "server asked for the
  password in cleartext inside TLS; prefer SCRAM"). Sin md5, GSS/Kerberos, SSPI,
  certificados de cliente ni tokens IAM.
- La contraseña, ASCII imprimible. 32 conexiones a la vez por máquina, 10 s para que el
  invitado mande arranque y contraseña, 15 s para toda la autenticación. Sin
  conexiones de replicación.
- En macOS, un `-upstream` con nombre se resuelve en el daemon al entregar la
  credencial, no en cada conexión, y se usa solo la primera IP (`localhost` es
  siempre el loopback, sin preguntar al DNS; ver la receta de la LAN).
- En macOS hay que recompilar `kling-vz`: el daemon rechaza una credencial con
  `-upstream`, `-upstream-tls` o `-tls-server-name` si su `/kling/info` no anuncia
  `postgres-upstream` en `credential_kinds`.

## Lo que no resuelve

- **El rol es el límite, no el proxy.** El proxy no mira el SQL: el invitado hace todo
  lo que el rol puede. Dale un rol de solo lectura, `GRANT` a lo justo, sin
  `CREATEROLE`, con `CONNECTION LIMIT`. Si el rol puede, el invitado también puede
  `ALTER ROLE … PASSWORD` y dejar la clave del proxy sin valor.
- **Sin TLS hacia el servidor, los datos van en claro** entre el proxy y el servidor.
  La contraseña no; las consultas y sus resultados, sí.
- **El tramo del invitado va en claro** dentro de la máquina: un cliente que exija TLS
  (`sslmode=require` o más, `channel_binding=require`) no conecta.
- Un upstream fijado se resuelve con el resolver del host: si ese DNS miente, el proxy
  marcará a donde diga, salvo a los rangos prohibidos. Si no te fías del DNS de la
  LAN, fija una IP.

## Credenciales guardadas antes de `-database` obligatoria

Antes, `-database` era opcional y sin ella el rol podía entrar en cualquier base. Ahora
hace falta `-database` o `-any-database` expreso. Los almacenes cifrados anteriores (máquinas
y plantillas) siguen cargando: una credencial Postgres guardada sin base se lee como
`any_database: true`, que es lo que permitía entonces, así que ninguna máquina viva se
rompe. No es silencioso: el daemon lo avisa una vez por máquina o plantilla y variable
(`postgres credential PGPASSWORD (db.example.com) loaded as any_database (pre-upgrade
store); rotate with -database`), `kling inspect` y `kling template inspect` la listan en
`credential_any_database`, y cada conexión sale en `kling machine audit` con
`(any_database)` tras `rol@base`. Para acotarla, rota la credencial con `-database`. Un `kling-vz` nuevo con un
daemon anterior sí rechaza una credencial sin base: actualiza los dos a la vez.
