# Changelog

Todas las novedades relevantes de kindling, del núcleo y de las extensiones
que viven en `ext/` (kling-mcp, kling-sandbox y el operador). Los binarios
pre-compilados están en [Releases](https://github.com/juan52878911/kindling/releases)
para linux/amd64, linux/arm64, darwin/amd64 y darwin/arm64; todos los de una
release son compatibles entre sí. Las novedades de kindling-mcp hasta v0.4.0 y de
kindling-sandbox hasta v0.2.2 están en [`ext/mcp/CHANGELOG.md`](ext/mcp/CHANGELOG.md)
y [`ext/sandbox/CHANGELOG.md`](ext/sandbox/CHANGELOG.md).

## Sin publicar

### Grafos desde los plugins

- **`kling db env up|down` y `kling db branch -env` (#58).** `env up <app-template>
  -golden G` crea un grafo `app` + `db` con una arista `credential` (el attach,
  declarado): la clave va por stdin al daemon, nunca por argv, entorno ni fichero, y si
  algo falla se deshace todo. `branch -env` lo da por rama de git y `branch -rm` lo
  borra. Solo Linux. Ver [`docs/db.md`](docs/db.md).
- **Agente + servidores MCP como grafo:** ejemplo `examples/grafos/agente-mcp.yaml`
  (validado por un test) y sección "Desde los plugins" en
  [`docs/grafos.md`](docs/grafos.md), donde también queda documentado, como siguiente
  paso, el gateway de IA (cascada Chispa a VON como grafo lazy).

### Núcleo

- **Autorización por operación en el socket del daemon (capacidad `authz`).** Con
  `/etc/kling/authz.json` (o `kling daemon -authz <ruta>`, `KLING_AUTHZ`), el daemon lee
  quién llama con `SO_PEERCRED` (Linux) o `LOCAL_PEERCRED` (macOS) —por SSH, el usuario
  remoto— y le da un rol por uid, usuario, gid o grupo: `admin` (todo) o
  `tenant:<nombre>`, que solo ve y opera las máquinas, snapshots y grafos con
  `kling.owner=<nombre>`. Esa etiqueta la pone el daemon al crear (run, sandbox, cada nodo
  de un grafo) y se hereda por commit, `run -from`, fork y snapshots de grafo; un
  inquilino no puede fijarla ni cambiarla. Lo ajeno responde 404, los listados y
  `/events` se filtran, `kling.db.owner` y `upstream_machine` quedan ligados al
  inquilino, y el proxy de Postgres exige el mismo `kling.owner` en copia y agente.
  Plantillas compartidas (`shared_templates`, solo lectura) y tokens de inquilino
  opcionales (`KLING_AUTHZ_TOKEN`). Volúmenes, carpetas del host, store, imágenes y
  métricas del host quedan para admin. Cada ruta declara su acción en una tabla única y
  un middleware decide. Sin fichero, todo como siempre, con un aviso en `kling doctor`;
  `kling info` y `GET /info` (`authz`) dicen el rol de quien pregunta. Un fichero pedido
  que falta, mal escrito o escribible por otros impide arrancar. Ver
  [`docs/authz.md`](docs/authz.md).
- **Proxy de credenciales para MySQL y MariaDB (`-type mysql`, #64).** El agente conecta
  en claro con su usuario y el marcador; el proxy manda su propio saludo, comprueba el
  marcador (`mysql_native_password` o la ruta rápida de `caching_sha2_password`, en
  tiempo constante contra todas las credenciales MySQL), abre TLS verify-full hacia el
  servidor (el saludo se lee sin buffer) y entra con la clave real
  (`caching_sha2_password`, completa dentro del TLS, o `mysql_native_password`). Sin TLS
  (`-upstream-tls disable`, solo con `-upstream`) solo valen los métodos que no mandan
  la clave. Nunca reenvía un error del servidor antes de autenticar, exige que el
  servidor hable el dialecto que eligió el invitado antes de mandar la clave y deja una
  línea de auditoría por conexión (`kind: mysql`). `KILL QUERY` no se mapea. Con
  credenciales Postgres y MySQL en la misma máquina, MySQL es el 3306 (en Linux, un DNAT
  propio a `n.HostIP:5382`). `kling-vz` anuncia `mysql` en `credential_kinds`. Ver
  [`docs/mysql.md`](docs/mysql.md).
- **Grafos de microVMs (`kling graph`, capacidad `graphs`).** Varias máquinas con
  nombre y aristas declaradas, descritas en un fichero JSON o YAML (un subconjunto sin
  dependencias), con ciclo de vida atómico: `up`, `ls`, `inspect`, `freeze`, `thaw`,
  `snapshot` (una plantilla por nodo, todas del mismo instante), `fork -n N` (grafos
  nuevos cuyas aristas llegan a sus propios nodos y nunca al original) y `rm`. Nodos
  `eager` o `lazy` (sin máquina hasta la primera conexión). Aristas `link` (TCP a un
  puerto de otro nodo por `<nodo>.graph`, por un proxy de enlace del daemon que resuelve
  en cada conexión y despierta al destino si duerme) y `credential` (el attach de
  Postgres de `kling db`, por dentro). El FORWARD entre namespaces sigue cerrado; el
  resolver de un nodo sirve solo los `*.graph` de sus aristas, también en `egress
  none`. Las conexiones quedan en la auditoría con `kind: link`. En macOS también, por
  el broker de enlaces (abajo). Nuevas rutas
  `/graphs`; las etiquetas `kling.graph*` y el espacio `graph` del store son del daemon.
  Ver [`docs/grafos.md`](docs/grafos.md).
- **Grafos: aristas `depends` y `share`.** `depends` ordena el grafo: un nodo no arranca
  ni despierta hasta que aquel del que depende corre (y, con `port`, ese puerto
  contesta); `up`, `thaw` y el despertar perezoso siguen ese orden, `freeze` y la pausa
  del snapshot el inverso, y un ciclo se rechaza al validar. `share` da a dos nodos la
  misma carpeta viva: es del grafo (`$KLING_ROOT/graph-shares/<id>`, se borra con
  `graph rm`), el destino la monta `rw` y el origen `ro` o `rw`; solo nodos con `image`,
  y un grafo con `share` no admite `snapshot` ni `fork` todavía (409). La arista `mcp`
  sigue fuera, ahora con el motivo: el puente MCP solo escucha en el 8080 del agente.
  `kling graph inspect` enseña las nuevas aristas. Ver
  [`docs/grafos.md`](docs/grafos.md#aristas-share-y-depends).
- **Aristas de grafo y `kling db attach` en macOS: el broker de enlaces** (#53). Antes,
  `501`. Ahora el `kling-vz` de cada nodo sirve `<nodo>.graph` en su DNS (solo los de
  sus aristas, en todos los modos) y, en cada conexión a una arista, pide la **arista**
  (no una dirección) al daemon por un socket Unix privado del usuario, el único que su
  sandbox le deja abrir. El daemon sabe qué máquina pregunta por el PID del otro
  extremo, comprueba bajo su candado como en Linux (despertando al destino si duerme),
  marca él mismo al reenvío del destino y le entrega el socket ya conectado; cortar es
  cerrar ese socket en los dos lados. Las credenciales Postgres con `upstream_machine`
  (aristas `credential` y `kling db attach`) van por el mismo camino, también en nodos
  sin `egress allowlist` si todas van a otra máquina. `kling-vz` lo anuncia con
  `graph-link` en `credential_kinds`; uno anterior no recibe aristas ni esas
  credenciales, y el error dice que hay que reconstruirlo. Nueva ruta de `kling-vz`
  `PUT /kling/graph` y paquete `pkg/linkbroker`. e2e: secciones 6g y 6h de
  `scripts/92-e2e-mac.sh` y `attach` en su 6f. Ver [`docs/grafos.md`](docs/grafos.md) y
  SECURITY.md §15.
- **`run -from` ya no copia entero el disco del dorado (`daemon.cow`).** La copia del
  overlay de cada instancia es un clon por reflink: con la raíz en XFS/Btrfs, FICLONE
  directo; en ext4, un almacén XFS propio (`$root/cow.xfs`, montado por loop en
  `$root/cow`, creado la primera vez y reservado entero) con una copia base por dorado y
  un clon por instancia. El coste de crear una instancia deja de depender del tamaño de
  su disco. `daemon.cow = auto | reflink-store | off` (`KLING_COW`) y
  `daemon.cow_store_gib`; sin reflink ni almacén posible, la copia de siempre con un
  aviso. El modo y los contadores salen en `kling info` (`disk clones`), en `GET /info`
  (`cow`) y en `kling doctor`. Lo existente no se migra. Medida:
  `scripts/bench-cow.sh`. Ver [`docs/cow.md`](docs/cow.md).
- **El almacén copy-on-write también puede ser Btrfs.** Donde el núcleo no tiene XFS
  (Proxmox visto desde un LXC) pero sí Btrfs, el almacén es `$root/cow.btrfs`
  (`mkfs.btrfs -K -m single -d single`, montado con `nodiscard` para no agujerear la
  reserva). Se elige por `/proc/filesystems`, XFS primero; un almacén existente conserva
  su tipo. El tipo sale en `kling info`, `kling doctor` y `GET /info` (`cow.store.fs`).
- **`commit`, `fork` y `graph snapshot` copian el overlay de la instancia desde el
  descriptor que comprobaron**, no reabriendo la ruta: un VMM ya no puede colar otro
  fichero en el dorado cambiando su overlay por un enlace entre la comprobación y la
  copia. El dorado se crea con `O_EXCL|O_NOFOLLOW` y se cede con `fchown`. En macOS el
  overlay de un commit se copia (disperso) en vez de clonarse.

### kling db

- **`kling db doctor -url` habla TLS (#86).** `pgmini` negocia TLS (SSLRequest + TLS 1.2+)
  y usa SCRAM-SHA-256-PLUS (`tls-server-end-point`) cuando el servidor lo ofrece; un
  servidor sin TLS nunca degrada a texto claro. `doctor -url` acepta `sslmode`
  (`disable|require|verify-ca|verify-full`, por defecto `verify-full`, contra las raíces
  del sistema más `sslrootcert` de la URL o `-ca-file`) y `-tls-server-name`. `sslmode=disable`
  (y `require`, que no autentica al servidor) solo con loopback, salvo `-insecure`
  explícito, y el informe lo avisa (DB041). `allow` y `prefer` se rechazan. Ya se puede
  revisar una base en la nube (RDS con SSL forzado) sin mandar nada sin cifrar.
- **MySQL y MariaDB en `kling db` (#64).** Plantillas MariaDB (Alpine) con
  `scripts/db-golden-mysql.sh` o `golden image|build -engine mysql`: datadir en el
  overlay, root solo por `unix_socket`, sin cuentas anónimas, `local-infile` apagado,
  `secure-file-priv` acotado y `server_audit` solo con `CONNECT`; la plantilla lleva
  `kling.db.engine=mysql`. `up`, `fork`, `connect` (`-mysql`, `-dsn` con `mysql://`),
  `rotate`, `reset`, `rm`, `branch`, `doctor` (reglas `MY001`-`MY054`) y `audit` (lee
  server_audit) funcionan sobre esas copias. La clave de cada copia se genera en el host
  y al invitado va su hash de `mysql_native_password`, nunca la clave, ni siquiera al
  construir la plantilla (se calcula con `openssl` o `python3`). `attach`, `role`,
  `rehearse`, `snapshot`/`undo`, `tenant-check`, `ask`, `clone` y `doctor -url` siguen
  siendo solo de Postgres y lo dicen. Ver [`docs/mysql.md`](docs/mysql.md).
- **`kling db` (extensión `kling-db`).** Bases Postgres desechables, una por microVM:
  `up`, `fork`, `connect`, `reset`, `rm`, `doctor`, `audit` y `golden`. Cada copia estrena
  clave (solo en el host; al invitado va el verificador SCRAM) antes de marcarse `ready`.
  `doctor` revisa la seguridad de una copia o de una URL; `audit` muestra sus conexiones
  sin SQL ni claves. Ver [`docs/db.md`](docs/db.md).
- **`kling db role <copia> -ro`** crea un rol de solo lectura dentro de la copia (sin
  escritura, sin pertenencias, con tiempos máximos y su propia clave en el host);
  `connect -role` lo usa. **`kling db templates`** y `golden build -template` construyen
  una golden de un comando (`empty`, `crm-demo`).
- **`kling db ask`.** Preguntas en lenguaje natural a una copia: el modelo (API de Anthropic)
  recibe solo el esquema y la pregunta y devuelve una SQL que se valida, se enseña y se
  ejecuta con un rol de solo lectura en `BEGIN TRANSACTION READ ONLY`. Ver
  [`docs/db-ask.md`](docs/db-ask.md).
- **`kling db ask-web`.** La misma garantía de `ask` en una página web mínima para quien no usa
  la terminal (HTML y JS embebidos, sin dependencias): solo esquema y pregunta hacia el modelo,
  la SQL se muestra y se ejecuta al pulsar un botón, con el rol de solo lectura en `READ ONLY`;
  `-explain` exige `-send-data`. Escucha solo en loopback (`-allow-remote` con aviso), token
  aleatorio en la URL que pasa a cookie `SameSite=Strict`, CSRF en cada POST, CSP sin inline,
  vida acotada (`-ttl`) y límite de peticiones. Ver [`docs/db-ask.md`](docs/db-ask.md).
- **`kling db rehearse`, `rotate`, `snapshot`/`snapshots`/`undo`.** `rehearse` ensaya
  migraciones SQL en una copia desechable (tiempos, esperas por locks, tamaño; un
  `lock_timeout` se informa como "would block"); `rotate` da una clave nueva a una copia y
  la vieja deja de valer; `snapshot` guarda una copia viva como punto de restauración y
  `undo` vuelve a él (mismo nombre y dueño, clave nueva). Ver
  [`docs/db.md`](docs/db.md).
- **`kling db branch`: una base por rama de git.** La copia de una rama nueva sale por
  fork de la de su rama padre (o del golden); `-switch`, pensado para el hook
  `post-checkout` (`branch hook install`), deja activa la copia de la rama actual,
  congela las demás (0 RAM) y escribe `DATABASE_URL` en `.git/kling-db.env` (0600, nunca
  en el árbol de trabajo). Además `-ls`, `-rm` y `-prune`. Ver
  [`docs/db.md`](docs/db.md#una-base-por-rama).
- **`kling db attach` / `detach` (modelo A).** Una copia compartida por
  agentes de otras microVMs, por el proxy de credenciales de Postgres de cada uno: el
  agente recibe un marcador y nunca la contraseña (la de la copia o la de un rol de
  `kling db role`). La credencial guarda el id de la copia (`upstream_machine`), no una
  dirección: el daemon la resuelve en cada conexión y solo si la copia sigue corriendo,
  lista y del mismo `kling.db.owner` que el agente. Congelar, parar o borrar la copia
  corta las sesiones abiertas. Nuevo `DELETE /machines/{ref}/credentials/{env}` y
  `kling machine credential -rm`; capacidad `db-attach`. En macOS, por el broker de
  enlaces (arriba). Ver [`docs/db.md`](docs/db.md).
- **`kling db tenant-check <copia>`: prueba de aislamiento entre inquilinos.** Nace del
  fallo de AuraCRM (políticas RLS con una rama `IS NULL OR = ''` que, sin inquilino,
  dejan ver todo). Dentro de la copia descubre las tablas con la columna de inquilino
  (`-column`, `tenant_id`), toma hasta `-max` valores (5) y, como el rol de la
  aplicación (`-role`, con `SET ROLE`), comprueba que sin inquilino (variable sin fijar y
  a `''`, `-setting app.tenant_id`) no ve ninguna fila y que cada inquilino solo ve las
  suyas; además intenta escribir filas de otro inquilino (INSERT, mover una suya, UPDATE
  de las ajenas) en transacciones que se deshacen. Informe por tabla con la política
  culpable y el porqué (las reglas de `doctor`), `-json` y salida 1 si algo falla, para
  CI. Los valores de inquilino nunca salen de la base ni se escriben en la SQL, y solo
  se imprimen recuentos. Ver [`docs/db.md`](docs/db.md#aislamiento-entre-inquilinos-tenant-check).
- **`kling db diff <copia1> <copia2> [-json] [-schema-only] [-max-rows N]`: qué cambió
  entre dos copias, sin volcar datos.** Esquema (tablas, columnas y tipos, clave
  primaria, índices, restricciones, RLS y políticas) y, por tabla, filas nuevas,
  borradas y cambiadas por clave primaria. Dentro de cada copia se calculan huellas por
  fila (md5 de la clave con una sal aleatoria de la ejecución y md5 de la fila); al host
  solo llegan huellas, ni claves ni valores. Tablas sin clave primaria: solo recuentos y
  un aviso. Más de `-max-rows` filas (100000) en una tabla: muestreo por huella de
  clave, igual en las dos copias y declarado en el informe. Las definiciones que se
  enseñan no llevan literales. Ver [`docs/db.md`](docs/db.md#diff-entre-copias-diff).

- **`kling db clone <postgres-url> -mask REGLAS [-golden G]`: un golden desde producción
  con los datos personales enmascarados.** La contraseña (de `PGPASSWORD` o
  `-password-stdin`, nunca de la URL) va al proxy de credenciales de una máquina de
  construcción con `-egress allowlist`: el invitado solo ve un marcador y el proxy entra
  por TLS verify-full (`-ca`, `-tls-server-name`; `sslmode=disable` solo con SCRAM). El rol
  tiene que ser de solo lectura (se comprueba antes de volcar; `-allow-writer` para
  saltarlo, nunca un superusuario). `pg_dump` corre **dentro** de esa microVM y se restaura
  por una tubería en un Postgres cuyo directorio de datos está en un tmpfs del invitado:
  el volcado sin enmascarar no toca nunca el disco del host. Allí se aplican las reglas
  (`email`, `name`, `phone`, `card`, `text`, `null`, `keep`, `fixed:<valor>`, por
  `tabla.columna` en JSON o YAML simple) en una transacción, con un hash con sal secreta
  de la construcción (se conservan relaciones y joins) y una comprobación de que ninguna
  columna enmascarada conserva un valor viejo; las columnas sospechosas por nombre
  (email, phone, name, dni, iban, card, address, ip…) sin regla **bloquean** la
  construcción salvo `-allow-unmasked` (`-strict` añade todo texto, JSON y array). Si algo
  falla, se destruye todo y no queda golden. El golden se construye con `db-golden.sh`
  desde el volcado ya enmascarado en una máquina nueva con `egress none`. El informe
  dice columnas tratadas, sospechosas y recuentos, nunca valores (`-json`). Ver
  [`docs/db.md`](docs/db.md#copia-de-producción-enmascarada-clone).
- **`kling db` en CI.** Scripts (`ext/db/scripts/ci-load.sh`, `ci-pr-db.sh`) y ejemplos de
  GitHub Actions y GitLab CI para una base por PR. Ver [`docs/db-ci.md`](docs/db-ci.md).
- **`sandbox fork -label k=v`.** Las etiquetas se aplican en el nacimiento de cada copia
  (sin ventana con las heredadas); `kling db fork` las usa para nacer en `preparing`.
- e2e: sección "kling db" en `scripts/90-e2e.sh` y `scripts/92-e2e-mac.sh`
  (`KLING_E2E_DB_GOLDEN`; se salta, avisando, si no hay plantilla), y 7f para `attach`;
  `clone` tiene su subsección en 7e (`KLING_E2E_CLONE_ADMIN_URL`, un Postgres con SCRAM
  en el host).

### Seguridad

- **El broker de enlaces de macOS no da direcciones.** `kling-vz` pide la arista y el
  daemon entrega el socket ya conectado tras comprobarla bajo su candado y volver a
  mirar que el destino no cambió al marcar: ni TOCTOU entre resolver y marcar, ni
  `kling-vz` marcando a los reenvíos de otras máquinas. Quién pregunta lo dice el
  kernel (`LOCAL_PEERPID`), no la petición; una credencial hacia otra máquina solo se
  atiende si está en el almacén de quien pregunta; el 8080 se rechaza también ahí; y
  hay topes por arista, por máquina y en el broker. Ver SECURITY.md §15.
- **El broker también mira el UID y el ejecutable de quien pregunta.** El otro extremo
  tiene que ser del usuario del daemon (`LOCAL_PEERCRED`) y ejecutar el `kling-vz` con
  el que el daemon arranca las máquinas (`proc_pidpath` por `proc_info`, sin cgo): un
  PID reciclado por otro programa ya no pasa. Como mucho 64 conexiones esperan a que su
  PID sea el de una máquina; el resto se rechaza en el acto.
- **Cuota por instancia en el almacén (#59) sin escape.** El overlay de cada instancia
  en el almacén es `root:grupo-del-VMM` 0660, no del VMM: el dueño de un fichero puede
  cambiarle el id de proyecto de XFS (`FS_IOC_FSSETXATTR`) y un Firecracker
  comprometido habría salido de su cuota o se habría comido la de otra instancia.
  `run -from` ya no hace `chown` a través del enlace de `machines/<id>`. Un `btrfs
  subvolume delete` que falla se reintenta y, si no, se avisa con el comando para
  borrarlo a mano. Ver SECURITY.md §14.
- **`kling db env` no adivina por el texto de un error.** Si un entorno existe lo dice
  `kling graph ls -json`, no un "404" o "not found" en el mensaje de un fallo.
- **Almacén de discos copy-on-write.** Cada jail recibe por bind solo el directorio del
  overlay de su instancia (nunca el almacén entero); el bind se desmonta antes de borrar
  el jail y, si no se puede, el jail no se borra. El almacén se monta
  `nodev,nosuid,noexec` y se reserva entero (sin sobreasignar). Ver SECURITY.md §14.
- **`kling db`: las copias nuevas no heredan los roles de `role -ro`.** `up`, `fork` y
  `undo` borran los roles con comentario `kling-db:ro` (y sus líneas de `pg_hba.conf`)
  antes de dar la copia por lista; si no pueden, la copia se destruye. La línea de
  `pg_hba.conf` de `role` abre solo la base de la copia, y el fichero se busca con
  `SHOW hba_file`. `ask` rechaza un rol que pertenezca a cualquier cosa salvo
  `pg_read_all_data` (también una pertenencia solo-SET de PG16). `rehearse` entra como el
  rol de la aplicación por peer, no con `role=` (que `RESET ROLE` deshacía).
- **Scripts de CI de `kling db`.** `ci-load.sh` ya no pone la clave en argv (`psql "$dsn"`):
  va por `PGPASSWORD`. `ci-pr-db.sh` deja de pasar `-ttl` a `reset` (fallaba siempre), borra
  con `kling db rm` y el mismo daemon, no usa `timeout` ni `grep -P` y lee `DATABASE_URL`
  sin fichero intermedio y con la traza apagada.
- **Fork: el almacén de credenciales falla cerrado.** Si no se puede mirar (cualquier error
  salvo "no existe") el fork se rechaza, y la comprobación de "sin credenciales" se repite
  con el cerrojo de la máquina justo antes de pausarla, para que un `SetCredentials`
  concurrente no se cuele. El mensaje ahora dice cómo hacerlo bien: "start another instance
  with run -from <template>".
- **kindling-sandbox reserva el prefijo `kling.db.`.** Un inquilino ya no puede fijar
  `kling.db.owner` ni `kling.db.state` al crear un sandbox.

- **Credenciales Postgres: `-database` obligatoria.** `kling machine credential` y
  `kling template credential` con `-type postgres` exigen `-database B` o, expreso,
  `-any-database` (nuevo campo `any_database` en la API; el CLI avisa por stderr de que el
  rol podrá entrar en cualquier base con `CONNECT`). Los almacenes anteriores sin base se
  leen como `any_database`, así que las máquinas vivas y las plantillas siguen cargando.
- **Contraseña en claro del servidor Postgres: visible.** Sigue admitida solo dentro de
  TLS verificado, pero la auditoría anota `auth: password` y el log del host avisa una
  vez por credencial ("server asked for the password in cleartext inside TLS; prefer SCRAM").

- **`kling volume create` ya no le da `volumes/` al usuario del VMM.** Recorría el
  directorio entero con `EnsureWritable`, que dejaba `volumes/` (y todo lo de dentro) con
  dueño el usuario sin privilegios del VMM hasta el siguiente reinicio del daemon, cuando
  `restringirRaiz` lo devolvía a root. Ahora solo cambia el dueño del fichero nuevo. Hacía
  falta para los snapshots de volumen: con `volumes/` del VMM, un VMM comprometido sin jailer
  (con jailer ni siquiera ve esa ruta) podía renombrar `volumes/snapshots/` y sustituir
  el pasado al que se vuelve.

- **Registro de auditoría del proxy de credenciales** (`kling machine audit <ref>
  [-f] [-denied] [-since 10m] [-tail N] [-json]`, `GET /machines/{ref}/credaudit`). Cada
  petición que pasa por el proxy, también cada rechazo, deja una línea en
  `machines/<id>/credaudit.jsonl`: método, host, ruta enmascarada (`:cred` donde hubiera
  un marcador o una forma de la clave, `:tok` en identificadores opacos largos), estado,
  motivo, si fue una denegación de política, qué credenciales se sustituyeron, bytes y
  duración. Nunca la clave, el marcador, cabeceras, cuerpos ni la query. 0600, abierto sin
  seguir enlaces, rota a 1 MiB (una generación); la escritura no bloquea la petición y lo
  que se descarta con la cola llena se cuenta en la línea siguiente. En Linux lo escribe
  el daemon, en macOS el `kling-vz` de la máquina (en el mismo directorio, sin RPC
  nueva); se lee con la máquina corriendo, congelada o parada, y sobrevive a
  freeze/thaw y a un reinicio del daemon. Son metadatos de tráfico que antes no llegaban
  a disco: ver SECURITY.md §7.
- **En macOS el rechazo del proxy fuera de allowlist es del propio proxy**
  (`credproxy.Options.Enabled`) y no de un envoltorio en `vnet`: cada denegación tiene un
  solo dueño y queda en el registro. El comportamiento para el invitado no cambia (403).

- **`-allow-request` rechaza rutas ambiguas en vez de normalizarlas a ciegas.** El proxy
  comparaba `-allow-request` contra la ruta ya decodificada y limpiada con `path.Clean`,
  pero eso asume que el proveedor lee `/`, `.` y `..` igual que nosotros. Con alguna
  credencial del dominio con `Allow`, una petición cuya ruta CRUDA lleve una barra o un
  punto codificados (`%2F`, `%5C`, `%2E`), una barra invertida literal, una barra doble, un
  parámetro de ruta con `;` o un segmento `.`/`..` sin decodificar recibe ahora 403 antes de
  normalizar y comparar, sin leer el cuerpo ni abrir la salida. Sin `-allow-request` no
  cambia nada.
- **Secretos por sesión de MMDS (`sessions[<id>]`) retirados.** El id de sesión se
  acuña en el bridge dentro del invitado (PID 1, root) justo al lanzar cada hijo, y
  como el bridge y el servidor MCP corren como root y leen el almacén MMDS completo,
  una sesión comprometida vería los secretos de todas las otras. El almacén sigue
  aceptando el campo para atrás-compatibilidad, pero se ignora: el bridge avisa UNA VEZ
  si está presente. Alternativas seguras: (1) `kling template credential` para secretos
  comunes en la plantilla; (2) `kling machine credential` (proxy de credenciales) para
  aislar claves por dominio; (3) VM efímera por sesión si necesitas secretos únicos por
  sesión. El aviso único, además de en un test unitario, se comprueba ahora de punta a
  punta en `ext/mcp/scripts/90-e2e.sh`: un kling-bridge real, con un almacén MMDS que
  trae `sessions` no vacío, avisa una sola vez en sus logs y el hijo arranca igualmente
  con las claves de `env`.
- **Proxy de credenciales: la clave de API ya no entra en el invitado.** Con un secreto
  por MMDS, un servidor MCP comprometido (corre como root) leía la clave y la sacaba por
  un dominio permitido: medido en el lab, la leía, la usaba y un eco de `httpbin.org` se
  la devolvía. `kling machine credential <ref> -domain D -env VAR` deja la clave en el
  daemon y le da al invitado un marcador; el resolver del modo allowlist desvía `D` a un
  proxy por máquina en el lado host del veth, que cambia el marcador por la clave solo
  hacia `https://D` y la redacta de las respuestas. Sin MITM: el SDK usa `http://D`
  hasta el proxy. Verificado desde dentro del invitado: el invitado solo ve el marcador,
  el proveedor recibe la clave real (httpbin basic-auth 200), HTTPS directo al dominio
  queda bloqueado, el eco llega redactado y otro Host recibe 403. Sin latencia añadida:
  90 ms de mediana frente a 363 ms por HTTPS directo con un cliente sin keep-alive (el
  proxy reutiliza su TLS). API: `POST /machines/{ref}/credentials`.
- **Las credenciales sobreviven al reinicio del daemon y al freeze/thaw.** Antes vivían
  solo en memoria: un reinicio dejaba el dominio sin resolver. Ahora se guardan cifradas
  en `machines/<id>/credentials.enc` (AES-256-GCM, clave derivada por HKDF de
  `secrets/snapshot.key`, atadas al id de máquina) y `reconcile` y `Thaw` las vuelven a
  entregar (proxy, resolver y marcadores en MMDS). Nunca en `state.json`, eventos ni
  snapshots: `commit` no copia ese fichero. Verificado en el lab: tras `systemctl
  restart kling` y tras freeze/thaw, el proveedor sigue aceptando la clave y el marcador
  es el mismo.
- **Una máquina con credenciales se puede congelar.** El marcador no es un secreto: la
  capacidad es la red del netns de esa máquina hacia su proxy, y el proxy sustituye por
  dominio. Ya no marca `HasSecrets`, así que el segador del gateway y el TTL funcionan
  con ella; MMDS de verdad sigue impidiendo congelar.
- **Credenciales de plantilla para servicios MCP.** `kling template credential
  <plantilla> -domain D -env VAR [-f clave | -clear]` ata la clave a la plantilla
  (`secrets/credentials/<plantilla>.enc`) y el daemon la entrega a cada instancia que
  nace de ella (`run -from`, réplicas del gateway) antes de devolverla, con un marcador
  propio por instancia; el puente lee MMDS al lanzar cada sesión, así que el servidor
  arranca con él en su entorno. Sobrevive a que `kling mcp import` rehaga el snapshot.
  `run -from` con otro egress se rechaza y dice qué pasar. API: `PUT
  /snapshots/{name}/credentials`; `Snapshot.credential_domains`. Verificado en el lab:
  dos instancias de la misma plantilla, cada una con su marcador, las dos con basic-auth
  200.
- **Proxy más robusto frente a un invitado hostil.** Se redacta también cada valor de
  cabecera tal y como salió sustituido —cierra el eco de un `Basic`, donde la clave iba
  dentro del base64 y el redactor no la veía— y las formas escapadas habituales de la
  clave (JSON `\/` y `\u00XX`, percent-encoding, entidades HTML); una respuesta con una
  codificación que no se puede inspeccionar (brotli, deflate) se rechaza con 502; varias
  credenciales por dominio (antes la segunda pisaba a la primera en silencio); el
  marcador se sustituye también en la query (`?key=`) y en el cuerpo (`client_secret`
  de OAuth; ver más abajo la sustitución en flujo); el redactor solo retiene del final de cada trozo lo que
  puede ser el comienzo de la clave, así que un flujo SSE sale evento a evento en vez de
  con la cola del anterior; repetir `-env` rota la clave conservando el marcador; y el
  443 de la IP del proxy también va al proxy, con lo que un `https://dominio` desde
  dentro muere en 3 ms en vez de esperar al plazo del SDK (un REJECT con RST necesitaba
  `xt_REJECT`, que el CT del lab no tiene: la regla fallaba y la máquina no arrancaba).
- **El proxy de credenciales ya no corta los streams largos.** Antes cada petición tenía
  120 s en total (contexto, `http.Client.Timeout` y `ReadTimeout`/`WriteTimeout` del
  servidor), así que un SSE de un LLM que durase más se cortaba a medias. Ahora hay tres
  plazos: 60 s hasta las cabeceras, 120 s de inactividad (cada byte en cualquier sentido
  los renueva) y un techo de 15 min por petición contra un invitado que gotee bytes para
  retener una plaza. Un corte aborta la conexión, así que el SDK ve un error y no una
  respuesta truncada que parece entera. Probado en tests con plazos reducidos: un stream
  de 1,2 s con un plazo de inactividad de 0,4 s llega entero, uno que se para se corta a
  los 0,3 s y el techo corta a los 0,6 s.
- **El marcador se sustituye en cualquier cuerpo de petición.** Antes solo en cuerpos de
  hasta 1 MiB con longitud declarada; uno chunked o mayor se reenviaba sin tocar. Ahora
  se sustituye en flujo con la ventana del redactor puesta al revés, leyendo el cuerpo a
  trozos de 4 KiB. Lo retenido EN MEMORIA por petición sigue siendo como mucho 1 MiB: si
  el cuerpo ya sustituido cabe en él, sale con su `Content-Length`.
- **Un cuerpo grande con Content-Length ya no sale chunked.** Si tras sustituir el
  marcador el cuerpo pasa de 1 MiB, antes salía chunked aunque el invitado hubiera
  declarado `Content-Length`, y hay proveedores de API que rechazan una subida chunked.
  Ahora, cuando el invitado SÍ declaró `Content-Length` (no llegó chunked), lo que pasa
  de 1 MiB se derrama a un fichero temporal (nombre aleatorio, 0600, en
  `$KLING_ROOT/tmp` si el daemon lo tiene o `os.TempDir()` si no, borrado al terminar la
  petición pase lo que pase) y se reenvía con el `Content-Length` exacto de ese fichero.
  Sin Content-Length (chunked del invitado) sigue saliendo chunked, que es lo que ya
  hacía: no hay una longitud que prometer de todos modos. Sin subir la memoria retenida
  por petición.
- **Permisos por método y ruta en cada credencial.** `-allow-request 'GET /v1/balance'`
  (repetible) en `kling machine credential` y `kling template credential`, y `allow` en
  la API. El método se compara exacto; en la ruta, `*` casa dentro de un segmento y `**`
  al final casa cualquier resto. La ruta de la petición se normaliza con `path.Clean`
  antes de comparar, y se reenvía normalizada. Si ninguna credencial del dominio casa, el
  proxy responde 403 sin leer el cuerpo ni abrir la salida. Sin `-allow-request` todo
  sigue permitido, y los almacenes cifrados de antes se leen igual.
- **IPv6 cerrado en los tres modos de egress, como defensa en profundidad.** El filtrado
  de `internal/net` (ipset, iptables, resolver dinámico) es solo IPv4; un diagnóstico en
  el lab real no encontró una fuga v6 hoy, pero por una razón incidental del host
  (`net.ipv6.conf.all.forwarding=0` de fábrica, no de kindling) y no por nada que el
  código garantizara. Dos capas nuevas, independientes entre sí: `ipv6.disable=1` en la
  línea de arranque del invitado (solo cubre arranques en frío; un dorado ya congelado
  no la relee) y, en el namespace del host y para los tres modos (`none`, `internet`,
  `allowlist`), `sysctl disable_ipv6=1` en `tap0`/veth más `ip6tables FORWARD DROP` como
  cinturón adicional si `ip6tables` está instalado (si no, se avisa y se sigue: la capa
  de `sysctl` es la que de verdad cierra el paso).
- **Proxy de credenciales también en macOS (backend vz), con los mismos permisos por
  ruta.** Lo sirve el `kling-vz` de cada máquina con el mismo `pkg/credproxy`: su DNS
  contesta el dominio con la pasarela (172.16.0.1) sin reenviar ni sembrar la IP real,
  un listener en pasarela:80 recoge la conexión antes de la política de salida y el 443
  de la pasarela muere con un RST. El daemon le entrega la clave por `PUT
  /kling/credentials` (incluye `allow`) y se la vuelve a entregar tras un thaw antes de
  cargar el estado; tras un reinicio del daemon no hace falta, porque el `kling-vz`
  sigue vivo con ella. Cambia el modelo de confianza: en macOS la clave vive en la
  memoria del `kling-vz` (mismo usuario que el daemon), el proceso que también termina
  el tráfico del invitado; SECURITY.md §7 cuenta qué supone. `vz/go.mod` requiere ahora
  el núcleo con `replace => ..`, como `ext/*`. Verificado en este Mac (M4) con
  `scripts/92-e2e-mac.sh`, sección 6c: el invitado solo ve el marcador, `httpbin.org`
  resuelve a la pasarela, basic-auth 200 con la clave real, eco y eco de `Basic`
  redactados, HTTPS directo rechazado en 1 ms, otro Host 403, la clave no está en el log
  del daemon ni en el de `kling-vz`, y sigue funcionando tras freeze/thaw (mismo
  marcador) y tras reiniciar el daemon.
- **Los dorados congelados antes de la barrera IPv6 avisan solos, en vez de quedar como
  un límite mudo.** `ipv6.disable=1` (arriba) solo se lee en un arranque en frío: un
  dorado ya congelado sigue con el módulo IPv6 del kernel del invitado cargado, aunque
  `applyIPv6Barrier` le cierre el paso igual en el namespace del host. Cada dorado graba
  ahora si se congeló con la barrera activa (`guest_ipv6_off` en su meta; deliberadamente
  fuera de la firma, como `kernel_sha256`: cubrirlo invalidaría la de todo dorado
  anterior al campo). Los anteriores no lo llevan y se leen como "no consta", nunca como
  "confirmado sin ella" — se heredan al hacer fork de un dorado, para no suponer "ya
  tiene la barrera" sin haber vuelto a arrancar en frío. `runFrom` avisa UNA VEZ por
  dorado sin la marca (log y evento `snapshot.guest_ipv6`, con cómo rehacerlo), y
  `kling snapshots` / `kling template inspect <nombre>` lo señalan.
- **Pendiente, documentado en SECURITY.md** ("Lo que NO está resuelto"): `-allow-request`
  no mira la query ni el cuerpo, y depende de que el proveedor interprete la ruta como
  `path.Clean`; en macOS la clave vive en la memoria de `kling-vz`, que también procesa
  el tráfico del invitado (cambio de modelo de confianza, aceptado a sabiendas); y los
  dorados de antes de la barrera IPv6 siguen con el módulo cargado en su kernel (avisado,
  no bloqueado — la barrera del namespace sí los cubre).

### Novedades

- **`fork` con etiquetas.** `POST /sandboxes/{ref}/fork` acepta `labels` y `kling sandbox fork`
  `-label k=v`: las copias nacen ya etiquetadas. Además, `api.Machine.Exposes(port)`.

- **Postgres en Docker o en la LAN/VPC: upstream fijado por el operador.** `kling machine
  credential` y `kling template credential` ganan `-upstream host:puerto` (a dónde marca
  el proxy en vez de `dominio:puerto`; IP o nombre, loopback y privadas permitidas),
  `-upstream-tls verify-full|disable` y `-tls-server-name` (el nombre del certificado si
  no es el dominio). Sin ellas nada cambia: IPv4 públicas y TLS verificado. Con
  `-upstream`, nunca se marca a `169.254.0.0/16`, `0.0.0.0/8`, multicast, `240.0.0.0/4`,
  `fe80::/10`, `fd00:ec2::254` ni a la red de kindling (`172.16.0.0/30`,
  `172.30.0.0/16`), tampoco si un nombre resuelve a alguna de ellas (en macOS, solo IP o
  `localhost`: `kling-vz` confinado no llega al resolver del Mac). `disable` exige
  `-upstream` y solo admite SCRAM-SHA-256 (ni `-PLUS`, ni contraseña en claro, ni md5, ni
  trust): la contraseña no cruza la red, las consultas sí (la CLI avisa si el upstream no
  es el loopback). La cancelación va al mismo upstream con el mismo modo, y la auditoría
  gana `upstream`. En Linux el proxy marca desde el netns del host; en macOS, `kling-vz`
  desde la pila del Mac (su `127.0.0.1` es donde publica Docker Desktop) y anuncia
  `postgres-upstream` en `credential_kinds`: sin él, el daemon no le da credenciales con
  estos campos (hay que recompilar `kling-vz`). API: `upstream`, `upstream_tls` y
  `tls_server_name` en `CredentialSpec` (omitempty; el almacén cifrado de antes se lee
  igual). Guía nueva: [docs/postgres.md](docs/postgres.md). e2e: `KLING_E2E_PG_UPSTREAM`,
  `KLING_E2E_PG_TLS` y `KLING_E2E_PG_SERVERNAME` en la 7d de `scripts/90-e2e.sh` y en la
  nueva 6e de `scripts/92-e2e-mac.sh`; `pglab`: `KLING_PGLAB_UPSTREAM` y
  `KLING_PGLAB_TLS=disable`.

- **Aislamiento por sesión en servicios MCP persistentes** (`kling mcp isolation <svc>
  [service|session]`, `kling mcp import … -isolation session`, anotación `mcp.isolation`).
  Hasta ahora el proceso era de cada sesión, pero el overlay era de la instancia: un
  fichero que una sesión dejaba en `/tmp` lo leía la siguiente (medido con
  `filesystem-mcp`). Con `session`, cada sesión MCP tiene su propia microVM, restaurada
  del snapshot dorado con su overlay. Se congela con la sesión dentro y la siguiente
  petición despierta esa misma máquina. El `DELETE`, `-session-ttl` sin uso (nuevo flag
  de `kling mcp serve`, 30 min por defecto) o el apagado del gateway la destruyen, y con
  ella su capa en el host. Un barrido recoge las que deja un gateway reiniciado. Tope:
  `-max-replicas` sesiones por servicio, reciclando la más ociosa si lleva 45 s sin uso.
  El agregador `_all` da una máquina por conversación. Medido en el lab x86: ~10 MiB de
  RAM y 0,3 MiB de disco por sesión despierta (135,8 MiB congelada, el volcado de memoria)
  y un primer `initialize` de **70 ms frente a 765 ms** de una sesión nueva en una
  instancia ya despierta, que paga node en frío porque ya gastó su hijo caliente. El valor
  por defecto sigue siendo `service`: es lo que quiere `memory`, cuyo grafo es de todas
  las sesiones. Con un volumen de escritura no se puede activar (un volumen tiene un solo
  escritor, y la segunda sesión no arrancaría). e2e: `ext/mcp/scripts/91-e2e-aislamiento.sh`.
  Diseño y límites: [`docs/aislamiento-por-sesion.md`](docs/aislamiento-por-sesion.md).

- **Proxy de credenciales de Postgres.** `kling machine credential <ref> -type postgres
  -domain db.ejemplo.com -user app [-database appdb] [-port 5432] [-ca-file ca.pem] -env
  PGPASSWORD` (y lo mismo en `kling template credential`; la clave, como siempre, por
  `-f` o stdin). El invitado conecta en claro al dominio del servidor, en el puerto que
  quiera, con el marcador como contraseña; el proxy entra en el servidor con la clave
  real por TLS verificado (SCRAM-SHA-256, con `-PLUS` si se ofrece; o contraseña en
  claro dentro de ese TLS) y solo entonces le da `AuthenticationOk`. Rol y base de datos
  atados a la credencial, `CancelRequest` traducido con claves falsas, nada sustituido en
  el flujo, una línea de auditoría por conexión (`kind: postgres`; `kling machine audit`
  la pinta como `PG rol@base`). Sin dependencias nuevas. En Linux escucha en
  `n.HostIP:5381` con un DNAT de todo puerto TCP de la IP del proxy salvo 53/80/443; en
  macOS lo sirve `kling-vz` en la pasarela, y el daemon exige que su `/kling/info` diga
  `credential_kinds: postgres` (hay que recompilar `kling-vz`). API: `type`, `port`,
  `user`, `database` y `ca_pem` en `CredentialSpec`; el almacén cifrado de antes se lee
  igual. Límites y lo que no resuelve (sin TLS en el tramo del invitado, `ALTER ROLE`,
  sin lista de SQL, bases en IP privada rechazadas): SECURITY.md §7 y "Lo que NO está
  resuelto". e2e: sección 7d de `scripts/90-e2e.sh` con `KLING_E2E_PG_URL`; prueba de
  laboratorio contra un PostgreSQL real con `-tags pglab` (`pkg/credproxy/postgres_lab_test.go`).
- **Snapshots de volumen y vuelta atrás.** `kling volume snapshot <vol> [nombre]` (por
  defecto la hora UTC), `kling volume snapshots <vol>`, `kling volume restore <vol> <snap>`
  y `kling volume rm <vol>@<snap>`; `volume ls` gana la columna `SNAPS`. Un snapshot solo
  se toma sin escritores y un restore solo sin ningún usuario (una máquina congelada
  cuenta); mientras dura, el volumen queda reservado en `volReservas` y un arranque que
  llegue a medias se rechaza con "being snapshotted"/"being restored". Restore guarda
  antes el estado actual en el snapshot reservado `undo`, así que se deshace con
  `restore <vol> undo`. Hasta 16 por volumen más `undo`; el gc no los toca, y
  `volume rm <vol>` se niega mientras haya alguno salvo `-snapshots`. La copia es
  `cp --reflink=always` en Linux (instantánea en XFS/Btrfs) con caída a copia completa
  dispersa, y `cp -c` (clonefile) en APFS; una copia completa que se comería el suelo de
  disco libre o el hueco hasta la marca alta del gc se rechaza con 507. Viven en
  `volumes/snapshots/<vol>/`, de root, `0700`/`0600`. API: `GET`/`POST
  /volumes/{name}/snapshots`, `POST /volumes/{name}/restore`, `DELETE
  /volumes/{name}/snapshots/{snap}`, `DELETE /volumes/{name}?snapshots=1`. Receta y
  nota sobre XFS en el README (Snapshots and rollback). No se expone como herramienta MCP.

- **kling-mcp: `kling-mcpbench` y `ext/mcp/scripts/95-thaw-scale.sh`, despertar a
  escala.** Un generador de carga (solo biblioteca estándar) que lanza M sesiones MCP a
  la vez tras una barrera contra N servicios a través del gateway —`initialize`,
  `notifications/initialized`, `tools/call`, K llamadas más y `DELETE`— y da p50/p95/p99/
  máx del primer resultado, errores por fase y código, y lo que le cuesta al host
  muestreando el `/metrics` del daemon y el PSI (microVMs vivas, réplicas, PSS, memoria
  disponible, tiempo hasta cero). El script monta la imagen y los servicios, su propio
  gateway y la matriz (N 1/10/50 × M 1/10/50/200, congelado/caliente, con y sin
  réplicas, R=3), salta las celdas que no caben en el 70 % de la RAM dejándolas en la
  tabla, marca DEGRADED por encima del 1 % de fallos y limpia comprobando la línea base.
  Método y reglas en [`docs/thaw-at-scale.md`](docs/thaw-at-scale.md); los resultados,
  pendientes de la pasada en el lab.

### Arreglado

- **kling-mcp: el gateway tiene tope de réplicas por servicio (`-max-replicas`, 16 por
  defecto).** No tenía ninguno (`MaxReplicas` 0 = sin tope): cada sesión que no cabía en
  las instancias existentes creaba una réplica, así que 200 sesiones simultáneas contra
  un servicio de 256 MiB (una sesión por instancia) intentaban 200 microVMs y el único
  freno era quedarse sin memoria en el host. Las sesiones por encima del tope reciben
  `503 ... all replicas full`, y el servicio ya no se anota como roto por ello (antes
  ese error era un 502 que marcaba su salud). `-max-replicas 0` devuelve el
  comportamiento de antes.
- **`kling run -from` hereda el egress de la plantilla si no se pide otro.** Mandaba
  siempre un egress (`none` por defecto), así que instanciar una plantilla con
  credenciales (`kling template credential`) exigía repetir `-egress allowlist -allow
  …` a mano en cada `run -from`, aunque el daemon ya sabe heredarlo del snapshot cuando
  llega vacío. Ahora, con `-from` y sin `-egress` explícito, el CLI manda vacío y deja
  que herede; con `-egress` dado, o sin `-from`, no cambia nada. De paso, el error de
  "necesita allowlist" ya lista los dominios concretos de la plantilla en vez de decir
  `<its domains>`.
- **El puente ya no pierde en silencio los secretos de MMDS.** `guest.FetchMMDS` devolvía
  `nil` tanto si el store estaba vacío como si no se podía leer, así que un secreto
  inyectado que el puente no alcanzaba acababa en una sesión que arrancaba sin él (y
  adoptando el hijo precalentado) sin dejar rastro. Ahora devuelve el error, el puente lo
  registra por sesión, y el cliente de MMDS ignora `HTTP_PROXY` del entorno.

### Pruebas

- `ext/mcp/scripts/90-e2e.sh`: la sección 8 crea su propia instancia del servicio, le
  inyecta el store y abre la sesión directamente contra su puente, en vez de depender de
  qué máquina elija el gateway. En el lab, dos pasadas seguidas: 25 ok, 0 fallos.

- `scripts/90-e2e.sh`: la sección 2 esperaba `warm` y el CLI dice `frozen`; la sección 5
  usaba `script -qec` (util-linux), que no existe en el `script` BSD de macOS desde donde
  se lanza el e2e: ahora el pseudoterminal lo pone el `pty` de python3. Sección 7 ampliada
  (freeze/thaw, reinicio del daemon, rotación, eco de `Basic`, HTTPS rápido) y nueva 7b
  (credenciales de plantilla). En el lab: 54 ok, 0 fallos. Nueva 7c: sonda dentro del
  invitado, en los tres modos de egress, de que no hay dirección ni ruta ni salida IPv6
  (sin ejecutar aquí — la corre quien tenga el lab a mano).
- `scripts/92-e2e-mac.sh`: nueva sección 6c (proxy de credenciales en el Mac, con
  freeze/thaw, reinicio del daemon y `kling-vz` confinado) y la misma corrección de
  `warm` por `frozen` en la sección 2. En este Mac (M4, `BURST=4`): 72 ok, 0 fallos.

- `scripts/90-e2e.sh`: nueva sección 3c (snapshots de volumen: escribe dentro del
  invitado, snapshot, rechazo con escritor y con congelada, snapshot con lector, restore,
  undo, permisos en disco y `rm` con y sin `-snapshots`). `scripts/92-e2e-mac.sh`: nueva
  6d, corta, que comprueba además que en APFS el modo es `clone`. Sin ejecutar aquí.

## v0.16.0 — 2026-09-27

Cierra lo que la auditoría del núcleo dejó abierto tras v0.15.0, y el tope de CPU llega a macOS.
Sin cambios de formato en `state.json`, `meta.json` ni la firma de los snapshots.

### Seguridad

- **Un invitado ya no alcanza los servicios del host por su IP pública.** Con salida
  (`internet` o `allowlist`), el tráfico de un invitado hacia una IP del propio host
  entraba por la cadena `INPUT`, que nadie filtraba: `sshd`, un gateway en `0.0.0.0`, la
  interfaz de Proxmox. En allowlist bastaba un dominio permitido que resolviera a esa IP.
  El daemon instala al principio de `INPUT`, para los veth `vh-*`, reglas que solo dejan
  pasar las respuestas a lo que abre el host y el DNS del allowlist. Reproducido y
  verificado en el lab con una IP pública de prueba.
- **`kling save` y `fork` se niegan con secretos inyectados.** Un secreto de sesión (MMDS)
  inyectado mientras se hacía un `save` o un `fork` acababa en el `mem.file` del dorado,
  el que mapean todas las instancias. `Commit` lo comprueba ahora con el cerrojo de la
  máquina tomado.
- **macOS: solo tu usuario llega al agente de un sandbox.** Los reenvíos de `kling-vz`
  escuchan en `127.0.0.1`, donde cualquier cuenta del Mac puede conectar, y detrás está el
  agente del invitado con exec y ficheros. `kling-vz` busca con `libproc` qué proceso
  tiene el otro extremo de cada conexión y corta las que no son de su usuario (~5 µs con
  caché, ~2,5 ms en el peor caso).
- **macOS: `kling-vz` se encierra en un perfil de sandbox** al crear o restaurar la VM:
  lee bajo la raíz de kindling, escribe solo en su máquina, `snapshots/` y `volumes/`, y
  sale a la red solo si la máquina tiene egress. Es el proceso que procesa el tráfico del
  invitado (pila de red, DNS, MMDS). `KLING_VZ_NO_SANDBOX=1` lo apaga para diagnosticar.
  Además nace con `umask 077`.

### Novedades

- **Tope de CPU en macOS.** `-cpu-pct` ya se aplica con el backend `vz`: `kling-vz` mide
  cada 100 ms la CPU del auxiliar de Apple donde corre la VM y la pausa lo justo para que
  la media quede en el techo, a partir de que el agente del invitado escucha. Medido en un
  M4 con un bucle infinito dentro: 96 % sin techo, 50 % con `-cpu-pct 50`, 25 % con 25.

## v0.15.0 — 2026-09-27

> **⚠ Cambio incompatible: jailer es obligatorio por defecto en Linux.** Tras actualizar,
> un daemon Linux sin el binario `jailer`, o que no corra como root, **deja de arrancar
> máquinas nuevas** —arranque en frío, restaurar un snapshot y también `thaw` de las
> máquinas warm— hasta instalar lo que falta (el error dice exactamente qué y cómo). Un
> daemon root con `jailer` pero sin el usuario de servicio (`kindling`, con el grupo `kvm`)
> sigue jaileando como root, con aviso. Para seguir sin jailer a propósito, arranca el
> daemon con **`KLING_JAILER=0`** (deja un aviso de seguridad en el log). Las máquinas
> ya en marcha, reanudar una pausada y los comandos de solo lectura no se ven afectados.
> Detalles en "Cambios incompatibles".

Ronda de remediación de una auditoría completa del núcleo (`internal/machine`,
`internal/net`, `internal/share`, `pkg/scheduler`, el agente invitado y el daemon):
carreras de verdad bajo `-race`, DoS del host por un invitado hostil, y varias mejoras de
arranque. Sin cambios en el formato de `state.json` ni en la firma de los snapshots.

### Seguridad

- **Recorrido de rutas en nombres de la URL.** El enrutador de Go desescapa `%2F` antes de
  casar la ruta, así que `DELETE /volumes/..%2Fimages%2Fmin` borraba la imagen base, y
  `GET /images/..%2Fvolumes%2Fdatos/files` leía un volumen ajeno. El daemon rechaza ahora
  con 400 toda ruta con una barra escapada, y los nombres de imagen y volumen (también los
  que llegan en el cuerpo, como la imagen de `POST /machines`, y la base que declara una
  receta) se validan donde se construye la ruta.
- **La memoria de las máquinas congeladas ya no la lee cualquiera.** Firecracker crea
  `mem.file` y `snap.file` con 0644 dentro de directorios 0755: cualquier cuenta del host
  leía la RAM del invitado. Los volcados quedan 0640 (también los que ya había, al arrancar
  el daemon) y `machines/`, `snapshots/`, `volumes/` e `images/` pasan a 0750 con el grupo
  del VMM, y `jails/` a 0700.
- **Un VMM comprometido ya no puede reescribir el kernel, las imágenes ni los dorados.**
  Eran propiedad del usuario del VMM (`kindling`), que es lectura y escritura para él: un
  Firecracker comprometido podía persistir en todas las microVMs futuras del host. Ahora
  son de `root` con el grupo del VMM (0640): los lee, no los escribe. Al restaurar un
  dorado con jailer, la ruta de su overlay dentro del jail lleva la copia propia de la
  instancia, así que el overlay dorado tampoco se abre nunca en escritura. Los enlaces del
  jail ya no ceden la propiedad del fichero original, no se siguen enlaces simbólicos al
  ajustar permisos, y las recetas (que pueden llevar secretos) quedan de `root` con 0600.
  Sin jailer (`KLING_JAILER=0`) el overlay de los dorados conserva la escritura por grupo,
  porque ahí Firecracker abre la ruta del host tal cual.

### Arreglado

- **Ciclo de vida de una máquina.** `fail()` escribía el error en una copia y la entrada
  viva se quedaba "running" con el PID muerto durante ~10 s hasta que el vigilante lo
  notaba. `rm`/`stop`/`freeze`, y la expiración por TTL, sobre una máquina que todavía
  está arrancando ahora **esperan a que termine de arrancar y actúan sobre el resultado**;
  antes `rm` podía dejar un VMM huérfano corriendo y el cliente igual recibía un 201. Una
  `Freeze` cuyo volcado falla y cuyo resume también falla ahora marca la máquina fallida
  al momento con el error real, en vez de quedarse "running" con un PID muerto y perder el
  error.
- **`kling save` (Commit) espera el cerrojo de la máquina** y tiene una única ruta de
  limpieza que corre en todo error (incluida una desconexión del cliente a media
  operación): la plantilla siempre acaba resumida y apuntando a su propio overlay, o
  marcada fallida — nunca pausada mirando al overlay dorado.
- **`snapshots rm`** puede responder ahora "in use right now … retry in a moment" mientras
  una instancia se está restaurando desde ese snapshot, en vez de dejar que la borradura
  y la restauración se pisen.
- **Los plazos de Firecracker para volcar/restaurar memoria** ya no son fijos a 30 s:
  escalan con la memoria (`10 s + 20 s/GiB`, nunca menos de 30 s). `Freeze`, `Thaw`,
  restaurar y `save` de máquinas grandes ya no expiran a media operación.
- **Consola serie acotada.** `firecracker.log` rota en el sitio al pasar de 16 MiB
  (conserva el último 1 MiB en `firecracker.log.1`); ya no puede llenar el disco del host.
  `kling logs` lee como mucho 4 MiB desde el final y nunca más de 10 000 líneas, sea lo que
  sea lo que pida `-tail`; el daemon rechaza con 400 un `-tail` que no sea un número o sea
  negativo, y el SDK acota su lado a 8 MiB de respuesta.
- **Resolver DNS del modo `allowlist` acotado**: como máximo 32 consultas a la vez y
  ~200/s (ráfaga 400) por microVM; por encima, SERVFAIL en el sitio, sin fork ni socket
  al upstream. La respuesta del upstream se valida (id de transacción + pregunta) antes de
  sembrar el ipset con ella.
- **Tope de descriptores por máquina en carpetas compartidas**: 2048 descriptores por
  máquina (repartidos entre todas sus carpetas vivas), respaldados por el tope global de
  16384 de siempre. Crear un fichero en una carpeta compartida comprueba primero con
  `Lstat` que no exista ya como directorio/enlace/FIFO/dispositivo y pide `O_EXCL` siempre.
  Subir varias copias en paralelo (`StageShareUpload`) ya no puede superar el tope de
  subidas pendientes por una carrera de comprobación-luego-reserva.
- **`/exec` legado del agente invitado**: cuerpo acotado a 1 MiB (413 si se pasa) y la
  salida combinada acotada a 64 MiB con un campo nuevo `truncated` (aditivo, no rompe
  clientes viejos) en vez de crecer sin límite en memoria. `PUT /files` ya no lo corta el
  timeout de lectura del servidor entero; tiene su propio plazo de 15 min, igual que el
  daemon.
- **Caches**: metadatos de snapshot, si una imagen soporta capas y el tamaño reservado de
  los `mem.file` inmutables de los snapshots dorados ya no se releen/recalculan en cada
  arranque o cada tick del vigilante.
- **Scheduler** (`pkg/scheduler`, usado por `ext/sandbox`): la cuota de instancias por
  tenant se reserva bajo el mismo cerrojo que la comprueba (dos arranques concurrentes ya
  no pueden pasar los dos la comprobación); `alive` consulta una instancia en vez de listar
  toda la flota; una instancia pausada cuya congelación falla al desalojarla vuelve al
  pool en vez de perderse; `KeepWarmAll` ya no apila una goroutine de más por tick por
  servicio frío.
- **Durabilidad**: el overlay de una plantilla, un volumen nuevo y una imagen de carpeta
  compartida se escriben en un temporal, se hace `fsync` y se renombran encima —
  publicación atómica, no una escritura a medias si el proceso muere en mitad de camino.
- **Ronda de verificación.** Con jailer bloqueado, `run`, restaurar y `thaw` se niegan
  antes de reservar nada (antes cada intento dejaba una máquina `failed` que contaba para
  el tope), y el error dice la causa real: daemon sin root, `-run-as` vacío, usuario
  inexistente, sin grupo `kvm` o sin binario `jailer`. El sello del volcado de
  una máquina warm guarda también `kernel_sha256` (opcional; los volcados anteriores
  siguen funcionando). El resolver DNS acota a 64 las conexiones TCP simultáneas y reintenta
  sembrar una IP si `ipset` falla. La cuota por tenant del scheduler ya no tiene una
  ventana en la que la instancia recién creada no cuenta. Además: la caché del tamaño de
  los `mem.file` dorados ya no hace un `stat` por acierto, borrar una imagen barre los
  temporales `.sha256-*` huérfanos, y una lectura tardía del cuerpo de una respuesta del
  invitado ya no puede escribir en un buffer que el daemon ya reutilizó.

### Novedades

- **`kling sandbox fork <sb> [-n N] [-ttl D] [-on-ttl remove|freeze]`**: ramifica un
  sandbox vivo en N copias independientes a partir de un snapshot temporal (pausa + dump +
  N restauraciones). Todo o nada: si una restauración falla, se deshace todo y el origen
  sigue corriendo. Nueva ruta `POST /sandboxes/{ref}/fork` y capacidad `fork` en
  `GET /info`.
- **Jailer obligatorio por defecto en Linux** (con salida explícita): ver "Cambios
  incompatibles".
- **Kernel mínimo propio**: `scripts/builders/kernel/build.sh` compila un kernel 6.1
  reproducible a partir de fragmentos Kconfig acotados (`config-common`,
  `config-amd64`/`config-arm64`) y `scripts/check-kernel-config.sh` verifica que ningún
  módulo (`=m`) ni opción prohibida se cuele. `KERNEL_SOURCE=build` en
  `scripts/30-fetch-artifacts.sh` lo usa en vez del kernel de CI de Firecracker
  (por defecto sigue siendo `ci`). Los snapshots dorados llevan ahora un
  `kernel_sha256` opcional en `meta.json`, y las máquinas congeladas en su sello:
  restaurar o descongelar sobre un host cuyo kernel cambió deja un aviso en el log del
  daemon. No se niega: ni restaurar ni descongelar usan `vmlinux` (el kernel del invitado
  va en su memoria); el aviso es para el siguiente arranque en frío, que sí usaría el
  kernel nuevo.
- **Arranque más rápido**: la línea de arranque del kernel incluye `quiet` y, en amd64,
  desactiva la emulación i8042 (PS/2); una máquina que arranca o se restaura corre a
  `max(cpu_pct, 100)` (hasta un núcleo entero) hasta que el agente invitado contesta (como
  mucho 10 s), y luego cae a su `cpu_pct` configurado; el diálogo SSH remoto reutiliza un
  socket `ControlMaster` (60 s de vida) en vez de abrir una conexión por llamada. Y el
  daemon deja de esperar dos segundos muertos al agente: el `tap0` de cada namespace reintenta
  ARP cada 50 ms en vez de cada segundo y cada intento de conexión tiene un plazo
  corto y creciente, así que ni la resolución ARP ni el SYN perdido de un invitado que
  aún arranca esperan su reintento de 1 s. Medido en el lab (i7-8700T, jailer): crear un
  sandbox de `toolchain` pasa de 2,12 s a 0,27 s en el daemon, y
  `kling try -- python3 -c 'print(1)'` desde un Mac por `ssh://` de 2,88 s a ~0,55 s.
- **`LICENSE`** (Apache-2.0) en la raíz del repo, y `NOTICE` la referencia.
- **`docs/benchmarks.md`** y **`scripts/bench-all.sh`**: cada cifra de rendimiento del
  README con su hardware, fecha aproximada y el script que la reproduce, marcando cuáles
  quedan obsoletas por los cambios de arranque de más arriba.

### Cambios incompatibles

- **Jailer pasa de opcional a obligatorio por defecto en Linux.** `kling run`/
  `kling daemon` se niegan a arrancar una máquina **nueva** (arranque en frío, restaurar
  un snapshot, `thaw`) si no encuentran el binario `jailer`, o si el daemon no es root y
  el usuario de servicio sin privilegios no está listo — antes caían en silencio a correr
  sin jailer. Un daemon root con `jailer` pero sin el usuario de servicio sigue jaileando
  como root, igual que antes, con un aviso de seguridad al arrancar. El arreglo es instalar
  jailer y el usuario (el mensaje de error dice los comandos exactos), o fijar
  `KLING_JAILER=0` para seguir sin él a propósito, lo que ahora deja un aviso de seguridad
  en el log del daemon al arrancar. Máquinas ya en marcha y comandos de solo lectura no se
  ven afectados. `KLING_JAILER=1` sigue forzando jailer, pero ahora falla con un error
  claro si el binario no está, en vez de un fallo de `exec` críptico.
- **`kling logs`** (y `Manager.Logs`/`Client.Logs`) ya no puede devolver más de 4 MiB /
  10 000 líneas aunque se pida `-tail 0` ("todo") o un `-tail` mayor que el tope; antes
  `-tail 0` leía el fichero entero sin condición.
- **`rm`/`stop`/`freeze` y la expiración por TTL** sobre una máquina que todavía está
  arrancando ahora bloquean hasta que el arranque termina (hasta ~2 min si esa máquina
  tiene carpetas compartidas en vivo que tardan en enganchar) en vez de devolver de
  inmediato sobre un estado a medio construir.
- **El resolver DNS de `allowlist`** puede responder SERVFAIL a un invitado que dispare
  ráfagas de más de 32 consultas simultáneas o más de ~200/s; antes no había tope y todo se
  reenviaba.
- **El tope efectivo de descriptores abiertos por máquina en carpetas compartidas** baja
  de hasta 8192 (8 carpetas × 1024 cada una) a 2048 compartidos entre todas las carpetas
  de esa misma máquina; el respaldo global de 16384 no cambia.
- **`snapshots rm`** puede fallar con "in use … retry" mientras algo se restaura desde ese
  snapshot; antes no lo comprobaba.
- Ninguno de estos cambios toca el formato de `state.json`, `meta.json` (el
  `kernel_sha256` nuevo es opcional y aditivo) ni la firma de los snapshots.

## v0.14.0 — 2026-09-26

**La CLI, ordenada, y el gateway de IA sin dominio.** Tres sustantivos —imagen (rootfs, arranca en frío) →
plantilla (snapshot dorado, arranca en ms) → máquina— y doce verbos de uso
diario en primer nivel; lo demás vive bajo su sustantivo o bajo la extensión
que lo aporta. Todos los nombres de antes siguen funcionando como alias
silenciosos.

### Núcleo

- **Pantalla `kling`**: sin argumentos (y `kling help`) enseña dónde empezar
  (`try`, `mcp add`, `connect`), lo de cada día, y a dónde ir a buscar lo
  demás, en 20 líneas. El volcado completo de siempre es `kling help all`.
- **Una sola ayuda** (`pkg/plugin/help.go`): `kling help <cmd> [<sub>]` y
  `kling <cmd> -h` imprimen lo mismo —sinopsis, `USAGE`, `FLAGS`, `See also`—
  para el núcleo y las extensiones. Arregla `kling help mcp` (daba
  `unknown subcommand "-h"`), `kling help try` (imprimía la sinopsis dos
  veces) y el `Usage of add:` crudo de las extensiones.
- **Estado `frozen`** en `ps`, `inspect`, `topo`, `top` y el API: es lo que
  produce `kling freeze`; hasta ahora se llamaba `warm`. **Es el único cambio
  en el JSON**: `state: "warm"` → `"frozen"`. La compatibilidad es de un solo
  sentido: quien lee con el `pkg/api` de 0.14 acepta también `"warm"`
  (`api.State.UnmarshalJSON`), así que un CLI 0.14 entiende a un daemon 0.13 y
  un daemon 0.14 lee el estado que guardó el 0.13; pero un lector de 0.13 (CLI,
  gateway o consumidor de `-json` propio) que reciba `"frozen"` de un daemon
  0.14 no lo reconoce como congelado: hay que actualizarlo. `-keepwarm`/
  `-prewarm` no cambian: describen una política, no un estado.
- **Nombres nuevos**: `save` (era `commit`), `template ls|inspect|rm` (era
  `snapshots`, `rmi`), `image …` (era `images`), `plugin …` (era `plugins`),
  `machine resize|squeeze|secret` (eran `resize`, `squeeze`, `mmds`; ocultos
  en `kling help`, en `help all` bajo ADVANCED con `topo`, `events`, `daemon`),
  `status -v` (era `info`). `kling status` sale con 1 si el daemon no contesta.
- **`ai model` y `ai chispa`**: `models` y `chispa` dejan de ser extensiones
  sueltas y pasan bajo `ai` (`kling ai model add`, `kling ai chispa train`);
  una sola incorporada, `ai`, con su manifiesto. `-vcpus` de `chispa deploy`
  pasa a `-cpus` como en todos los demás (el viejo avisa).
- **Unidades con sufijo** (`pkg/units`): `-mem 512M|2G`, `-ttl 10m|1h`,
  `-mem-max 2G`, en `run`, `try`, `sandbox`, `machine resize`, `ai model add`,
  `ai chispa deploy`, `mcp import`, `mcp verify`. Un entero desnudo vale lo de
  siempre (MiB, segundos): ningún script cambia de significado.
- **Listados y borrados iguales**: todo `<noun> ls` acepta `-q` (`template`,
  `image`, `volume`, `sandbox`, `mcp`); `rm`, `template rm`, `image rm`,
  `volume rm` y `sandbox rm` aceptan varios nombres, preguntan en una terminal
  si son más de uno y `-f` lo salta (sin terminal no preguntan). `ps` enseña
  `IMAGE/TEMPLATE`: una máquina instanciada de una plantilla se identifica por
  ella.
- **`next:`** en lo que crea algo (`run`, `save`, `sandbox create`,
  `volume create`, `ai model add`, `ai chispa deploy`, `up`), como `doctor`
  imprime `fix:` y los errores `try:`. Va a stderr. `NO_COLOR` vuelve ASCII
  las marcas ✓/!/✗ de `doctor`.
- **Extensiones con espacio de nombres** (manifiesto v2): los comandos de una
  extensión se teclean `kling <ext> <cmd>` y le llegan como `kling-<ext>
  <cmd>`; `top_level` promueve uno (solo `connect`); `hidden` lo deja fuera de
  la ayuda; `group` en el manifiesto dice en qué sección de la pantalla sale.
  Los manifiestos v1 (kling-mcp 0.13) se siguen leyendo con su semántica de
  antes, y los alias `add/search/gateway/…` solo se traducen si ninguna
  extensión instalada sirve ya esa palabra. El completado se genera del mismo
  árbol (`cmd/kling/tree.go`), con tercer nivel para `machine resize <ref>`.
- **Alias silenciosos** de todos los nombres de antes; plan: en 0.15 avisan una
  vez por proceso, en 0.16 se retiran los de extensiones; `commit`, `snapshots`
  y `plugins` se quedan para siempre.
- **Descubrimiento**: `kling-vz`, `kling-chispa`, `kling-bridge`,
  `kling-bridge-local`, `kling-guest` y los `companions` de las extensiones
  instaladas ya no se toman por extensiones (`kling doctor` enseñaba
  `✗ extension vz`). `doctor` solo avisa del directorio de extensiones fuera
  del PATH si hay compañeros dentro, y da por bueno el completado instalado en
  el rc aunque esta shell no lo haya cargado.
- **`scripts/install.sh`**: escribe cada binario en un temporal del mismo
  directorio y lo renombra encima (sobrescribir un `kling` en marcha lo mataba
  en macOS por la firma de código); detecta la shell para la línea de recarga
  del completado (siempre decía zsh; también en `kling plugin ls|install`);
  instala el completado y, salvo `--no-rc`/`KLING_NO_RC=1`, añade a tu rc la
  línea que lo carga y el PATH de `--prefix` si faltaba, para que
  `kling doctor` salga en verde nada más instalar.

- **Tareas de intención genéricas en el gateway de IA.** El bloque `"domotica"`
  de `ai.json` y el tipo de tarea `domotica` desaparecen del núcleo: en su lugar,
  una tarea `"intent"` (`/v1/decide`, `kling ai test|eval|retrain`) con la
  misma cascada —plantillas → Chispa + Chispa-slots (en proceso o serverless) →
  codificador con puerta de McNemar → `escalate: "von"`— y la misma mejora
  continua, pero sin vocabulario de ningún dominio: la cascada vive en el paquete
  nuevo `pkg/intent` y lo que sabe el dominio (plantillas, valores de los huecos,
  qué necesita cada intención) sale de un esquema JSON (`"schema"`) o de un
  `intent.Domain` en Go que registra el programa que embebe `pkg/aigw`
  (`"domain"`, `aigw.Options.Domains`). Guía en `docs/intent.md`.
- **Cambio incompatible** para quien tenga una tarea `domotica`: el bloque pasa a
  `"intent": {"model": …, "domain" | "schema": …, "slots", "encoder", "head",
  "encoder_force", "final_oos"}` (`intent` → `model`); `kind` de `/v1/tasks` y
  del registro de evaluación es `"intent"`, y un registro `kind: "domotica"` ya
  no enciende la capa 3: hay que repetir `kling ai eval <tarea>`.
- Fuera `pkg/domotica` y la pista «`kling plugin install domotica`».

### kling-mcp

- Comandos bajo `kling mcp`: `search`, `add`, `import`, `ls` (con `-q`),
  `inspect` (nuevo), `refresh`, `refresh-bridge`, `verify`, `health`, `heal`,
  `link`, `unlink`, `serve` (era `gateway`), `export`, `memory`, `migrate`;
  `connect` sigue en primer nivel. `kling-mcp gateway` y `kling-mcp mcp …`
  siguen funcionando para las units de 0.13; `kling-gateway.service` y
  `kling-heal.service` usan ya `serve` y `heal`.

### kling-sandbox y operador

- `sbx` se promueve explícitamente (`top_level`): bajo el nombre de la
  extensión sería `kling sandbox …`, que es del núcleo. El operador y el
  frontal leen `frozen` (y `warm` de un daemon anterior).

### Ejemplos

- **La demo de domótica es un programa aparte, `kindling-domotica`**, y no una
  extensión de kling: sin subcomando `kling domotica`, sin asset
  `kling-domotica-<os>-<arch>` en la release ni `install.sh --with domotica`.
  Todo lo de la habitación (taxonomía, léxico, plantillas, simulador,
  validación del LLM, datos) vive en `examples/domotica`
  (`internal/domotica`, `internal/tools`, `cmd/domotica-data`, antes
  `pkg/domotica` y `tools/domotica-data`). Subcomandos: `gateway` (el gateway
  de kindling con el dominio `smart-room` registrado), `room` (la página, antes
  `domotica-demo`) y las herramientas `decide`, `eval`, `train-slots`, `embed`,
  `train-encoder`, `templates`, `eval-llm`. `make domotica` lo compila; los
  servicios de systemd de `examples/domotica` arrancan `kindling-domotica
  gateway` y `kindling-domotica room`. Las decisiones y las cifras de
  `docs/DOMOTICA-EVAL.md` no cambian (`kindling-domotica eval` da la misma
  tabla, y `/v1/decide` la misma respuesta en las 9 794 frases del test).

### Docs

- README, `docs/*.md`, las guías de `ext/mcp` y `ext/sandbox`, los scripts de
  e2e y los benches usan los nombres nuevos. `docs/extensions.md` describe el
  manifiesto v2 y el plan de alias.

## v0.13.0 — 2026-09-26

**Un repositorio, una release.** kindling-mcp y kindling-sandbox entran en este
repositorio como extensiones, en `ext/mcp` y `ext/sandbox`, con su historial, y
salen en la misma etiqueta que el núcleo. **Todos los binarios de una release son
compatibles entre sí**: se acabaron las tablas de compatibilidad. Las
extensiones se instalan desde el propio `kling`:

```sh
kling plugins install mcp        # o sandbox, o domotica
kling doctor
```

Desde esta versión cada release trae las subsecciones `### Núcleo`,
`### kling-mcp`, `### kling-sandbox y operador` y `### Ejemplos`. Los CHANGELOG
de `ext/mcp` y `ext/sandbox` quedan congelados con su historia hasta v0.4.0 y
v0.2.2.

### Núcleo

- **Extensiones en el mismo repo.** Tres módulos Go más `vz/`, unidos por un
  `go.work` comiteado: el raíz (`github.com/juan52878911/kindling`, sigue con
  cero dependencias y sin cgo, guardado por el CI), `ext/mcp` y `ext/sandbox`,
  que dependen del núcleo con un `replace => ../..` que está siempre (así
  `GOWORK=off` y el Dockerfile del operador compilan igual). Un cambio en
  `pkg/api` rompe la compilación de las extensiones en el momento, no en la
  siguiente release. Cada módulo tiene `make test` (gofmt, vet, `test -race`) y
  `make cross`; el CI los recorre en matriz.
- **Una release con todo** ([`docs/releases.md`](docs/releases.md)): además de
  `kling`, `kling-guest`, `kling-chispa` y `kling-vz`, cada etiqueta publica
  `kling-mcp`, `kling-bridge`, `kling-sandbox` y `kling-domotica` para
  linux/darwin × amd64/arm64, `kindling-operator` para Linux,
  `kindling-mcp-host.tar.gz`, `kindling-sandbox-host.tar.gz`,
  `kindling-operator-deploy.tar.gz` y un único `SHA256SUMS`. La imagen del
  operador sigue en `ghcr.io/juan52878911/kindling-operator`.
  `scripts/install.sh --with mcp,sandbox,domotica` instala núcleo y extensiones
  de una vez.
- **`kling plugins install <n>[@vX] [-from URL] [-file RUTA] [-sha256 H] [-dir DIR]`**:
  baja `kling-<n>-<os>-<arch>` de la release de kindling que corresponde al
  propio `kling` (o a `@vX`), lo **verifica contra el `SHA256SUMS` de la release
  antes de escribirlo**, comprueba su manifiesto y lo deja en el directorio de
  extensiones (primer directorio de `$KLING_PLUGIN_PATH`, si no
  `$XDG_DATA_HOME/kling/plugins`, si no `~/.local/share/kling/plugins`), con un
  `kling-<n>.json` al lado (`name`, `version`, `url`, `sha256`, `installed`).
  `-from` solo acepta `https://`. Un `kling` de desarrollo exige `@vX`, `-from`
  o `-file`. Solo biblioteca estándar.
- **`kling plugins ls [-json] | rm <n> | enable <n> | disable <n>`**: `ls` enseña
  `STATUS` (`ok`, `disabled`, `error: …`) y el origen con ruta y sha256 corto
  (y avisa si el binario cambió después de instalarlo);
  `rm` borra binario, `.json` y companions del directorio de extensiones (y no
  toca nada fuera de él); `disable` apaga una extensión sin desinstalarla
  (`"plugins": {"disabled": [...]}` en `config.json`): se lista pero no recibe
  comandos. Vale también para las incorporadas.
- **Manifiesto: nuevo campo opcional `companions`** (`manifest_version` sigue
  en 1), ejecutables que se instalan y se borran
  junto a la extensión sin ser extensiones (`kling-bridge` para `mcp`). El resto
  del manifiesto no cambia.
- **`ai`, `chispa` y `models` son extensiones incorporadas** (`plugin.Builtin`):
  siguen dentro del binario, pero su ayuda y su completado salen de un
  manifiesto, `kling plugins ls` las lista como `built in` y
  `kling plugins disable ai` las apaga (cada una por su nombre).
- **`kling domotica` sale del núcleo**: sus subcomandos (`decide`, `eval`,
  `train-slots`, `embed`, `train-encoder`, `templates`, `eval-llm`) los sirve
  la extensión `kling-domotica` tras `kling plugins install domotica`, con la
  misma sintaxis. Sin ella, `kling domotica` dice qué instalar. `pkg/domotica`
  se queda en el núcleo porque la tarea `/v1/decide` del gateway de IA lo usa.
- **Comandos nuevos para el día a día:**
  - `kling doctor [-json]`: daemon, versión del CLI frente a la del daemon,
    cada extensión con su estado y su `min_kling`, si el completado está
    cargado y si el directorio de extensiones está a mano, además del runtime
    (`up -check`). Cada ✗ trae el comando que lo arregla. Sale con 1 solo si hay
    fallos: los avisos (completado sin cargar, versiones distintas) no cuentan.
  - `kling try [-image I] [-from S] [-mem MiB] [-egress …] [-keep] [--] <cmd…>`:
    crea un sandbox, ejecuta el comando en streaming, devuelve su código de
    salida y lo borra, también con Ctrl-C; sin comando abre una shell; `-keep`
    lo conserva e imprime su id. Sin `-image` ni `-from` usa la imagen
    `toolchain`.
  - `kling logs -f <ref>`: sigue la consola hasta Ctrl-C o hasta que la máquina
    deja de estar `running`; funciona también contra daemons anteriores.
  - `kling snapshots ls|rm|inspect <n> [-json]`: los snapshots tienen por fin
    los mismos verbos que `volume`, `images` o `sandbox`; `inspect` los enseña
    con sus anotaciones. `kling rmi` queda como alias.
  - `kling help <cmd>`: solo la ayuda de ese comando, no la entera.
  - `kling ai up [-config ai.json] [-listen ADDR]`: el gateway de IA con
    `./ai.json` o `~/.config/kling/ai.json` en `127.0.0.1:8080` por defecto,
    imprimiendo la URL y un `curl` de prueba antes de servir.
  - `kling completion fish` y `kling completion install [shell]`, que escribe el
    script en `~/.config/kling/` y dice qué línea añadir al rc (no lo toca).
- **`-json` en `sandbox ls`, `context ls`, `config show` y `version`**, con la
  forma de `ps -json`. **`kling version`** enseña también la versión del daemon,
  para no descubrir el desfase por un 404.
- **Errores con el siguiente paso**: cuando se sabe qué hacer, el error trae
  una segunda línea `try: …` (`kling doctor` si el daemon no responde,
  `kling ps -a` ante una máquina desconocida, `kling images ls`,
  `kling snapshots ls`, `KLING_SOCKET_USER` si el socket no deja entrar,
  `kling plugins install <x>` ante un comando que se mudó a una extensión,
  `kling plugins enable <x>` ante uno de una extensión desactivada). Un comando
  desconocido ya no vuelca la ayuda entera.
- El completado gana `ai up`, `models embed`, `domotica embed` y
  `domotica train-encoder`, que faltaban, y los subcomandos de
  `plugins`, `snapshots` y `completion`.
- **`scripts/install.sh --with mcp,sandbox,domotica`** baja las extensiones de
  la misma release, verificadas contra su `SHA256SUMS` antes de mover nada, al
  directorio de extensiones con su `kling-<n>.json`. Opciones nuevas:
  `--skip-kling`, `--plugin-dir` y `--no-companions`; `--bridge` pasa a ser
  `--with mcp`.
- **CI en matriz** (raíz, `ext/mcp`, `ext/sandbox`), cada módulo con
  `make test cross` sin workspace (`GOWORK=off`, Go 1.24), más comprobaciones
  de que el `go.mod` raíz no tiene dependencias ni cgo, de que `ext/*` conservan
  `replace => ../..` y de que el `go.work` compila. Antes de publicar, la
  release compara sus ficheros con la lista fija de assets; una ejecución
  manual exige la etiqueta.
- `make cross`, `make test-all`, `make cross-all` y `make domotica` en la raíz;
  `scripts/release.sh` se niega a etiquetar si algún `ext/*/go.mod` no pide el
  núcleo en la versión que se etiqueta.

### kling-mcp

- **Vive en [`ext/mcp`](ext/mcp)** (módulo
  `github.com/juan52878911/kindling/ext/mcp`), importado de kindling-mcp v0.4.0
  con su historial: `git log -- ext/mcp` enseña los commits originales. Mismos
  comandos, mismos snapshots y mismo gateway; cambia la forma de instalarlo:
  `kling plugins install mcp`, que trae `kling-bridge` como companion. La parte
  de host sale en `kindling-mcp-host.tar.gz` o con `make -C ext/mcp deploy`.
- Sus versiones van con las de kindling: esta es kling-mcp v0.13.0. El repo
  kindling-mcp queda archivado, con sus releases
  ([`docs/archivo-repos.md`](docs/archivo-repos.md)).
- Su manifiesto declara `kling-bridge` como companion, y `kling mcp link`
  busca el puente primero junto a `kling-mcp`, que es donde lo deja
  `kling plugins install`.
- `ext/mcp/scripts/install.sh` delega en el instalador del núcleo
  (`--skip-kling --with mcp`) y conserva sus opciones antiguas.
- Los tests que buscaban un clon de kindling en `KINDLING_DIR` usan `../..` por
  defecto y ya no se saltan.

### kling-sandbox y operador

- **Vive en [`ext/sandbox`](ext/sandbox)** (módulo
  `github.com/juan52878911/kindling/ext/sandbox`), importado de
  kindling-sandbox v0.2.2 con su historial: `kling plugins install sandbox`, y
  la unidad del frontal en `kindling-sandbox-host.tar.gz`.
- `kindling-operator` se publica como binario Linux, como
  `kindling-operator-deploy.tar.gz` con los manifiestos de `ext/sandbox/deploy/`
  y como imagen `ghcr.io/juan52878911/kindling-operator:v0.13.0` y `:latest`,
  construida desde la raíz del repo
  (`docker build -f ext/sandbox/Dockerfile.operator .` o
  `make -C ext/sandbox operator-image`). La guía pasa a
  [`docs/kubernetes.md`](docs/kubernetes.md).
- El repo kindling-sandbox queda archivado, con sus releases.
- El operador ya no crea dos veces el mismo sandbox en el frontal cuando el
  resync reconciliaba una foto de la caché tomada antes de que la
  reconciliación del watch terminara de crearlo: `reconcile` relee el Sandbox
  de la caché ya dentro de su candado.

### Ejemplos

- **[`examples/hello-extension`](examples/hello-extension)**: la extensión
  mínima, un `main.go` con `plugin.Main`, un comando, una clave de
  configuración y el gancho de `status`, con un test que ejecuta su
  `--kling-manifest` y lo valida. Es el hilo de
  [`docs/extensions.md`](docs/extensions.md), reescrita como "escribe tu
  extensión en 10 minutos": el código, dónde instalarla, cómo probarla, el
  manifiesto campo a campo con `companions`, y cómo publicarla para
  `kling plugins install -from`.
- **`examples/domotica/cmd/kling-domotica`**: la extensión de domótica, y la
  referencia de una extensión con varios subcomandos de verdad.
- **`examples/mcp`**: `echo`, `stdio-server`, `notas-server` y `agent.py`, que
  venían de kindling-mcp, ahora en el módulo raíz (solo biblioteca estándar).

## v0.12.0 — 2026-09-25

### Ejemplo: triaje de fallos de CI (`examples/ci-triage`)

- **Programa aparte** que usa kindling solo por fuera (`kling chispa train` y
  el gateway por HTTP): Chispa puntúa cada línea de un log de CI fallido
  (Travis, `gh run view --log-failed` o texto), elige el trozo que explica el
  fallo y le pone categoría; VON solo lee ese trozo, con `json_schema`
  (`{category, summary, next_step}`), y solo cuando Chispa duda. `analyze`,
  una página local (`serve`) para confirmar o corregir, `eval` con líneas base
  y logs propios anotados a mano (`lines`, `-manifest`), y `export` de las
  confirmaciones (`ci-triage.feedback/v1`) para `kling chispa train` o la
  importación de etiquetas humanas de la mejora continua. Logs acotados (la
  cola de 16 MiB, 100 000 líneas).
- **Datos**: LogChunks (MSR 2020, CC BY 4.0), descargado por
  `examples/ci-triage/build-data.sh` y fijado por sha256; reparto por
  repositorio; categorías de reglas en entrenamiento y revisadas a mano en
  prueba (`data/test-categories.tsv`); modelos idénticos byte a byte.
- **Cifras** ([docs/CI-TRIAGE-EVAL.md](docs/CI-TRIAGE-EVAL.md)): en 160 logs de
  16 repos no vistos, el trozo de Chispa toca el fallo anotado en el 70 % de los
  logs (últimas 30 líneas: 54 %; regex: 38 %), a 25 ms de mediana por log y
  ~1 µs por línea; categoría 0,52 (VON Qwen2.5-1.5B 0,56, no significativo;
  0,5B empeora a 0,43). Fuera de dominio (34 fallos reales de GitHub Actions)
  empata con la cola del log (56 %). En microVM, 7,6× más lento que en
  proceso y sin keep-alive agota los puertos del Mac: por línea, en proceso.
- `kling chispa deploy -reuse-image`: hace el dorado de una imagen que ya
  existe (en macOS, la construida en Linux y traída con `kling images copy`),
  en vez de negarse.

### Domótica: Chispa serverless en la capa 2

- La tarea de domótica (`/v1/decide`) acepta un modelo de intención
  `"backend": "microvm"`: la capa 2 pregunta a la réplica de `kling chispa
  deploy` por el mismo camino validado que `/v1/classify` (etiquetas del
  registro de despliegue, confianza del lado del gateway). En proceso sigue
  igual.
- `kling chispa deploy -slots` graba también los huecos del `.chispas` en el
  registro; la réplica marca los huecos en la misma ida y vuelta y el gateway
  los valida (nombre, posición dentro del texto, orden, tope) y rehace su
  texto. Sin huecos en el registro se ignoran y los marca el `.chispas` local.
- La decisión trae `chispa_replica` (`frozen`/`paused`/`warm`/`new`,
  `wake_ms`, `request_ms`) y la traza de la demo lo enseña en el paso de
  Chispa; un fallo de la réplica escala con `reason: "chispa_error"`.
  `/v1/tasks` dice `chispa_backend`.
- `examples/domotica/ai.json` sirve la capa 2 serverless (la variante en
  proceso, en su README). Medido en el CT 105: congelada 34 ms (thaw 29 ms),
  pausada 3,6 ms (resume 0,8 ms), despierta 0,6–0,9 ms en `/v1/decide`;
  mismas cifras de `kling ai eval room` que en proceso.

### Despertar más rápido: de 152 a 27 ms congelada, 2,2 ms pausada

Medido fase por fase en un i7-8700T (KVM sin anidar, jailer) con una tarea
Chispa de 128 MiB detrás de `kling ai serve`; cada palanca con su antes/después
y lo que no funcionó, en [docs/despertar.md](docs/despertar.md).

- **Desglose por fases**: `thaw` devuelve `wake` (`api.WakePhases`: wait, check,
  net, spawn, socket, load, resync, cgroup, total) y `kling thaw` y `kling
  events` lo enseñan; el planificador lo completa (`scheduler.WakeTrace`: list,
  renew, wake, ready) y el gateway de IA añade la primera petición, en su log y
  en `/metrics` (`kling_ai_wake_phase_seconds{model,how,phase}`).
  `scripts/99-thaw-bench.sh daemon|gateway|paused` lo mide.
- **La red sobrevive al freeze**: namespace, veth, tap y reglas se quedan
  montados y el thaw los reutiliza (47 → 0 ms); el vigilante los suelta pasados
  30 min y un reinicio del daemon, como siempre. Cuando hay que rehacerla, cuesta
  la mitad (el veth nace en su namespace, `ip -n`, reglas en un solo
  `iptables-restore`). La MAC del tap0 es fija.
- **La memoria no se lee a golpe de fallo de página**: freeze deja en la caché
  el volcado de las máquinas pequeñas (≤ 128 MiB) y thaw lee en segundo plano el
  de las de hasta 512 MiB (resync 54 → 6 ms, primera petición 14 → 2,5 ms).
- El VMM nace ya en su cgroup (`CLONE_INTO_CGROUP`, 8 → 0 ms), el socket de su
  API se sondea cada milisegundo (11 → 4 ms) y el chroot del jail se borra al
  congelar, no al descongelar.
- **Nivel pausada**: `kling pause` / `POST /machines/{ref}/pause` (capacidad
  `pause`) deja el VMM vivo con el invitado parado; `thaw` lo reanuda en ~0,3
  ms. `kling ai serve -paused-mib 256` (por defecto; 0 = congelar siempre) pausa
  en vez de congelar las réplicas ociosas que mejor puntúan por popularidad /
  memoria mientras quepan, congela de verdad las que pasan `-paused-for` (10 ×
  idle) sin uso, y las sacrifica primero si falta memoria. Réplica pausada →
  decisión: 2,2 ms, con ~36 MiB de RSS por réplica Chispa.
- `kling ai serve -name-prefix` para el nombre de las réplicas.
- Arreglo: el cliente de la API de Firecracker dejaba una conexión abierta por
  llamada; con varias pausas sobre el mismo VMM su API acababa rechazando la
  siguiente (`write: broken pipe`).

### Mejora continua: Chispa aprende lo que escalaba (`kling ai retrain`)

Diseño, puertas y cifras en [docs/mejora-continua.md](docs/mejora-continua.md).

- **Captura opt-in por tarea** (bloque `learn` del registro): cada respuesta
  lleva un `id`, y lo que Chispa escala se guarda con su texto filtrado de
  secretos (o solo su hash), el top-k de Chispa, la versión que lo dijo y los
  votos de quien contestó después (VON en la cascada, con `von_votes` de
  autoconsistencia; el codificador en domótica). Escritor en segundo plano,
  sin bloquear la petición; almacén acotado por tamaño y días.
- **`POST /v1/feedback` y `kling ai feedback`**: etiquetas humanas (confirmar,
  corregir, descartar; solo el token principal habla como persona) y votos de
  maestros externos (`ext:<nombre>`, nunca verdad por sí solos); `-import`
  para lotes JSONL de otras herramientas.
- **`kling ai review`**: la cola para una persona, con una auditoría al azar
  (20 % de las capturas por hash) que es lo único que valida a los maestros, y
  lo más informativo primero; `-i` interactivo.
- **`kling ai retrain`**: oro entero + humano + lo de maestros validados que
  pasa el filtro de acuerdo (con peso y tope por clase), sombra con los mismos
  hiperparámetros, y promoción solo si gana en el conjunto de confianza
  (McNemar sobre «contesta bien», precisión confiada sin bajar). Guarda de
  fugas, versiones `@vN.chispa` con `.prev` y swap atómico; en microvm, un
  dorado `<snapshot>-vN` verificado por sha256. Vuelve a evaluar la cascada si
  estaba activa. **`kling ai rollback`** vuelve al instante.
- **Métricas**: cobertura por versión en vivo, tasa de escalado en ventana,
  cobertura y precisión por versión en el conjunto de confianza, escaladas y
  tiempo ahorrados (estimados); sección nueva en `kling ai ls`.
- `kling chispa train`: campo **`weight`** por ejemplo en el JSONL.
- Medido con el gateway real y maestros simulados en los datos de domótica:
  cobertura en el conjunto de confianza de 0,588 a 0,689 en cuatro rondas con
  la precisión confiada plana (0,976–0,981) y 750 etiquetas humanas; un LLM
  malo forzado como maestro da un modelo peor (contesta bien 0,676 → 0,625) y
  la puerta lo rechaza (en las tres semillas medidas).

### VON más rápido en CPU

Cada cambio con su banco de pruebas y su puerta (entra solo si mejora lo medido
sin empeorar la calidad); lo que no funcionó, también contado. Todo en
[docs/von-cpu.md](docs/von-cpu.md).

- **Prefijos de tarea precalculados en el dorado.** Las imágenes de `kling
  models add` arrancan `llama-server` con una caché de prompts de 64 MiB
  (`-cache-ram`, que se suma a la memoria de la VM; 0 la quita), y el dorado se
  congela con el system prompt de cada tarea ya evaluado: `kling models add
  -prefix system.txt` (repetible) o **`kling ai prime`**, que los saca del
  registro del gateway (el `system` de cada tarea y el texto fijo de su plantilla)
  y rehace el dorado de cada modelo VON (etiqueta `von.prefixes`; sin cambios, no
  hace nada). Medido con un system prompt de ~800 tokens: la primera petición de
  una réplica recién restaurada pasa de 4,7 s a 0,38 s en Qwen2.5-1.5B y de 1,7 s
  a 0,17 s en Qwen2.5-0.5B; alternar dos tareas en la misma réplica, de 2,8 s a
  0,11 s por petición. Una imagen anterior se reutiliza con `-cache-ram 0` (solo
  queda el último prefijo). No aplica a los codificadores (kind `embed`,
  [docs/codificador.md](docs/codificador.md)): cada petición es una frase
  corta y distinta, así que su spec fija `-cache-ram 0` siempre y `kling models
  add -prefix` / `kling ai prime` los rechazan con un mensaje claro.
- **`json_schema` por tarea** en las generaciones del gateway: la salida de VON
  se restringe a JSON que cumple el esquema (de 19/21 a 21/21 respuestas válidas
  en Qwen2.5-1.5B, de 11/21 a 21/21 en 0.5B), y el gateway contesta 502 si aun así
  no es JSON (p. ej. cortada por `max_tokens`).
- **Q4_0 en el catálogo** para `qwen2.5-0.5b-instruct` y `qwen2.5-1.5b-instruct`:
  en ARM llama.cpp la reempaqueta para i8mm y evalúa el prompt ~1,8× más rápido
  que Q4_K_M (1,5B) o genera ~35 % más rápido que Q8_0 (0,5B), sin acertar menos.
  La cuantización por defecto no cambia (x86 sin medir).
- `scripts/97-von-cpu-bench.sh` y `scripts/von-bench/`: el banco (tarea de
  domótica con respuestas esperadas, primer token, cambio de tarea, tok/s,
  aceptación del borrador, validez del JSON).
- **No entró**: la decodificación especulativa (borrador Qwen2.5-0.5B para 1.5B,
  SmolLM2-135M para 360M y 1.7B, y n-gramas) fue igual o más lenta en todas las
  configuraciones medidas, incluso con un 96 % de aceptación; tampoco hilos
  distintos del número de vCPU, `--poll 0`, lotes mayores ni la caché KV en Q8_0.

### Domótica: capas rápidas de decisión (`kling domotica`)

Diseño en [docs/domotica.md](docs/domotica.md), datos y licencias en
[docs/domotica-datos.md](docs/domotica-datos.md), cifras en
[docs/DOMOTICA-EVAL.md](docs/DOMOTICA-EVAL.md).

- **`kling domotica decide "<texto>"`** decide qué hace la habitación de demo
  (luces, termostato, persianas, tele, cerradura, alarma, ventilador, altavoz,
  enchufe) en español o inglés: `{intent, slots, layer, confident, latency_us}`.
  Capa 1, órdenes de la demo por coincidencia normalizada (1,5 µs, sin
  reservas); capa 2, intención Chispa + huecos Chispa-slots (~5 µs la cascada). Lo
  indirecto, lo fuera de ámbito, las órdenes múltiples o incompletas escalan
  (`escalate: "encoder"`). También `eval` (frente a plantillas y reglas, con
  frases de reto), `train-slots` y `templates`.
- **Chispa-slots** (`pkg/chispa/slots`): etiquetador de secuencias lineal
  (perceptrón estructurado promediado + Viterbi BIO) sobre características
  hasheadas, pesos int16, determinista; formato `.chispas` con el endurecimiento
  del `.chispa` (topes antes de reservar, CRC-32C, `FuzzLoad`).
- `pkg/domotica`: taxonomía de 28 intenciones, léxico es/en, números con
  palabras y unidades, plantillas estilo hassil, emparejador, cascada.
- `tools/domotica-data`: descarga fijada por sha256 de Amazon MASSIVE 1.0 y
  home-assistant/intents (ambos CC BY 4.0, atribución en `NOTICE`) y los
  convierte a un esquema único con repartos sin fugas.
- `chispa.FoldRune` se exporta para que otros extractores plieguen igual que Chispa.

### VON en hierro x86, sin anidar

- Primeras medidas de VON y Chispa en x86 bare metal (i7-8700T, Firecracker sobre
  KVM nativo, sin la virtualización anidada del laboratorio Lima): thaw y
  primer token bajan a milisegundos y la generación llega a la velocidad real
  de la CPU (48 tok/s en SmolLM2-360M, frente a 8,8 anidado); el binario
  oficial de llama.cpp para amd64 funcionó a la primera. Palancas de
  `llama-server` medidas sin código nuevo de kindling (`-threads`,
  `--cache-type-k`, decodificación especulativa, `--slot-save-path`, Q4_0);
  cifras y método en [docs/von.md](docs/von.md#x86-sin-anidar-i7-8700t).

### Domótica: capa 3, el codificador de frases

Diseño, cifras y la receta del ajuste fino en
[docs/codificador.md](docs/codificador.md); evaluación en
[docs/DOMOTICA-EVAL.md](docs/DOMOTICA-EVAL.md#capa-3-el-codificador).

- **Codificadores en el catálogo de VON** (kind `embed`):
  `multilingual-e5-small` (MIT) y `paraphrase-multilingual-minilm-l12-v2`
  (Apache-2.0), Q8_0. `kling models add enc-e5 -model multilingual-e5-small`
  construye con el mismo constructor `llm` (`llama-server --embeddings
  --pooling mean`) y congela un dorado calentado con frases reales; `kling
  models embed <réplica> "<texto>"`. Réplica de 512 MiB: ~3 ms por orden en un
  Mac M4; en Linux, +10 MiB de PSS por réplica de más.
- **`scripts/encoder-gguf.sh`**: nadie de confianza publica su GGUF, así que se
  convierten con el conversor de llama.cpp b11147, todo fijado (pesos por
  sha256, código, `uv`, paquetes) y reproducible bit a bit; validado contra
  transformers (coseno 1,00000 en F16, ≥ 0,999 en Q8_0). El constructor toma un
  GGUF convertido de su caché por hash.
- **`pkg/codificador`**: cabeza (regresión logística o una capa oculta) sobre
  los vectores congelados, en Go puro, determinista, int16, calibrada con la
  temperatura y los umbrales por clase de Chispa; formato `.jenc` endurecido
  (`FuzzUnmarshal`), caché de vectores `.jemb`, cliente de `/v1/embeddings`
  acotado, k-NN de comparación.
- **Cascada**: `Decider.Encoder` (capa 3) y `DecideContext`; lo que el
  codificador tampoco resuelve escala a `"von"`. `kling domotica embed`,
  `train-encoder`, y `-encoder`/`-embed-url`/`-embed-cache` en `decide` y
  `eval`. Bate la marca: MASSIVE exact 0,745 es / 0,800 en (0,718 / 0,782), 2
  errores confiados en el reto; lo indirecto sigue siendo de VON.
- **Gateway**: tareas `domotica` en `/v1/decide`, modelos `kind: "embed"`
  despertados y congelados por `pkg/scheduler`, y `kling ai eval` de la tarea,
  cuyo registro enciende la capa 3 solo si contesta bien más órdenes sin más
  errores confiados.
- `pkg/domotica/indirect.jsonl`: 180 órdenes indirectas escritas a mano con
  reparto train/valid/test (`train-encoder -indirect`, y el test en `eval`).
- `scripts/98-encoder-bench.sh` (latencia y memoria de un codificador) y
  `scripts/encoder-setfit/` (ajuste fino contrastivo con GPU: receta sin
  ejecutar).
- **Mejora futura, no aplicada:** el ajuste fino con GPU de arriba resolvería
  el lenguaje indirecto dentro de la capa 3 en vez de escalarlo a VON; decisión
  de no lanzarlo por ahora y detalle (coste, tiempo, alternativa en Mac con
  MPS) en [docs/codificador.md](docs/codificador.md#mejora-futura-no-aplicada-ajuste-fino-con-gpu).
### Chispa serverless: tareas en microVMs congeladas (`kling chispa deploy`)

Hasta ahora Chispa solo vivía dentro del proceso del gateway (`kind: "chispa"`,
microsegundos, sin daemon). Ahora es también una tarea serverless de kindling,
igual que un VON: `kling chispa deploy <tarea> -model m.chispa [-slots s.chispas] [-mem
64] [-vcpus 1]` empaqueta el `.chispa` con un invitado nuevo, estático y sin cgo,
`cmd/kling-chispa` (carga el modelo al arrancar y sirve `/v1/classify` y
`/healthz`), y congela un dorado con el constructor `chispa` nuevo (mismo motor que
`llm`: capa sobre la base `min`, sin nada que descargar). En el registro del
gateway, `"backend": "microvm"` en vez de `"path"` hace que la tarea la sirva
esa réplica —despertada y congelada por `pkg/scheduler`, como a un VON— en vez
de este proceso; `"backend": "inprocess"` (o nada) sigue siendo la opción de
siempre. Detalle, diagrama y cifras en
[docs/chispa-serverless.md](docs/chispa-serverless.md).

- Medido en un i7-8700T (Proxmox CT 105, KVM sin anidar, backend Firecracker):
  imagen de 13 MB en disco (capa sobre `min`), dorado de 45 MB; primer arranque
  desde el dorado (`restore`) 1,41 s, **thaw de una réplica congelada + primera
  decisión, 135-140 ms**; con la réplica ya despierta, mediana 0,57-3 ms según
  concurrencia y hasta 4231 decisiones/s a 16 clientes por el gateway (HTTP +
  microVM). El mismo modelo en proceso, en el mismo host: mediana 91-123 µs y
  hasta 26 924 decisiones/s a 4 clientes.
- **Carga bajo demanda de modelos Chispa en proceso, medida** (Mac M4): RSS ocioso
  con 0/10/100 modelos en el registro (nada cargado todavía) 15,5/18,3/28,3 MiB;
  con un presupuesto de memoria (`-chispa-mem`), el LRU desaloja de verdad (20
  modelos usados, presupuesto de 8 MiB → 5 quedan cargados); hasta 113 000
  decisiones/s agregadas a 32 clientes por el mismo camino HTTP en proceso.
- `pkg/aigw`: `ModelConfig.Backend` (`inprocess` | `microvm`), validado; la
  cascada Chispa → VON funciona igual desde una réplica microvm (candidatos y
  evidencia enteros, no un top-3, para que `top_k` no pierda etiquetas).
- **No entró de esta rama:** el barrido de 1/10/50 tareas microvm simultáneas en
  el backend `vz` del Mac y la comprobación de compartición de páginas entre
  réplicas del mismo dorado en Linux se quedan pendientes (requieren volver a
  entrar en el host de pruebas; ver docs/chispa-serverless.md).

### Domótica: capa 4 (un LLM con salida JSON) y la habitación de demo

Diseño en [docs/domotica.md](docs/domotica.md#capa-4-un-llm-con-salida-json),
cifras en [docs/DOMOTICA-EVAL.md](docs/DOMOTICA-EVAL.md#capa-4-el-llm-von), la
demo en [docs/demo-domotica.md](docs/demo-domotica.md).

- **Capa 4** (`pkg/domotica`): lo que las capas 1–3 escalan va a un LLM VON por
  una tarea de generación del gateway (`POST /v1/generate`) con un esquema JSON
  (`kind`, `reply` y hasta 4 `actions` con intención, dispositivo, zona, valor
  y color de la taxonomía). La respuesta se valida estrictamente y se ancla a
  la frase (zona, color y número que la frase nombra; nunca abrir la puerta ni
  desarmar la alarma sin decirlo); si algo falla, no se hace nada y se pide
  aclaración. Un veto no deja convertir en orden lo que el modelo rápido da por
  fuera de ámbito, salvo lo indirecto. Varias órdenes en una frase: 8 de 9 bien.
- **`kling domotica eval-llm`** compara la capa 4 con «escalar y no hacer nada»
  (McNemar y la cascada entera ponderada con lo que no es para la habitación),
  en dos alcances, y escribe el registro que la enciende. Con
  Qwen2.5-1.5B Q4_K_M pasa solo donde el modelo rápido duda: 31 órdenes más
  bien en MASSIVE, ninguna acción fuera de ámbito (errores confiados 1,7 → 1,9 %);
  preguntándole por todo, actúa en el 1,5 % de la charla y empeora la cascada.
- `domotica.Cascade` con capas enchufables (`Layer`, `FastFunc`), su traza por
  capa (`Trace`) y los adaptadores al gateway (`GatewayClient`: `/v1/decide`,
  `/v1/generate`, `/v1/tasks`, despertares de `/metrics`).
- **La habitación de demo** es un ejemplo aparte, [`examples/domotica`](examples/domotica/README.md):
  página embebida (sin CDN) con el plano en SVG, órdenes de ejemplo y texto
  libre, traza por capa con latencia y el despertar de cada microVM, panel de
  microVMs por capa con su memoria (del daemon) y contadores; simulador de
  dispositivos en Go con SSE; español e inglés, claro y oscuro, accesible;
  loopback por defecto, cuerpos acotados, CSP estricta. Registro de ejemplo del
  gateway (`ai.json`) y unidades de systemd para el servidor x86.

### Modelos VON: LLM pequeños bajo demanda (`kling models`)

Diseño, uso y cifras en [docs/von.md](docs/von.md).

- **`kling models add <nombre> -model <id> [-quant q8_0]`** construye con una
  orden la imagen de un modelo —`llama-server` de llama.cpp y el GGUF, como capa
  sobre una base Debian trixie— y su **snapshot dorado**, congelado con el modelo
  ya cargado y caliente. `kling run -from <nombre>` da réplicas que contestan en
  cuanto termina el thaw, con la API compatible con OpenAI en el puerto 8000
  (`/v1/chat/completions`, `/v1/models`, `/health`, `/metrics`). También
  `models ls`, `models ask <réplica> <prompt>` (respuesta y tokens/s) y `models rm`.
- Catálogo fijado por revisión y sha256: `smollm2-360m-instruct` (Q8_0, Q4_K_M),
  `qwen2.5-0.5b-instruct` y `qwen2.5-1.5b-instruct` (Q8_0, Q4_K_M; los de 1,5B con
  4 vCPU); o un GGUF propio de Hugging Face con
  `-url …/resolve/<commit>/<fichero>.gguf -sha256 …`. llama.cpp `b11147`, binarios
  oficiales con todas las variantes de CPU, verificados por sha256. Mandos:
  `-ctx` (2048 por defecto), `-parallel`, `-threads`, `-cpus`, `-mem`, `-cpu-pct`.
- **Constructor `llm`** (`kling builder llm`, instalado por `make deploy`): descarga
  y verifica en Go, cachea por hash en `$KLING_ROOT/cache/von` y se hace su base
  glibc (`glibc-trixie`, con `71-build-glibc-base.sh`) la primera vez; necesita
  `debootstrap` en el host. `models add -build-only` construye solo la imagen,
  para copiarla a un daemon de macOS y hacer allí el dorado.
- El dorado se hace con un core por vCPU (`cpu_pct` 100 × vCPU) y devuelve al
  host lo reclamable (`squeeze`) antes de congelar. Las réplicas del mismo dorado
  muestrean con semillas distintas (medido).
- **Solo licencias abiertas en el catálogo por defecto** (Apache-2.0, MIT): cada
  entrada lleva su licencia y dónde leerla (`kling models ls`). `qwen2.5-3b-instruct`
  (Q4_K_M, Qwen Research License, no comercial) está para evaluar y solo se
  construye con `-accept-license qwen-research`.
- `pkg/von`: catálogo, validación, cliente mínimo de la API y `MakeGolden`. El
  calentamiento del dorado se reintenta mientras quede `-wait`: en un host lento
  la primera respuesta de un 1,5B pasa del plazo de 5 min del proxy del daemon.
- `81-base-image.sh` acepta `ROOTFS_DIR` (ficheros que copiar a la imagen) y
  `SERVICE` (un ejecutable que el entrypoint arranca y relanza antes del agente).
- `scripts/96-von-bench.sh`: arranque en frío y thaw hasta el primer token,
  tokens/s, memoria de 1..N réplicas y semillas.
- Arreglos de camino: `kling run -from` hereda el `cpu_pct` del snapshot (antes
  caía al 50 % de un core salvo que lo pasara quien llamaba, como hace el
  planificador), y el daemon deja legible para el VMM la base que un constructor
  cree (antes solo la imagen construida).

### Chispa: un clasificador lineal diminuto

`kling chispa train|eval|predict|inspect` y los paquetes `pkg/chispa` (características,
formato, inferencia, calibración) y `pkg/chispa/train` (entrenador). Go puro, sin
dependencias ni cgo; corre en local, sin daemon. Diseño en
[docs/chispa.md](docs/chispa.md), evaluación en [docs/CHISPA-EVAL.md](docs/CHISPA-EVAL.md).

- Características: palabras y bigramas normalizados (minúsculas Unicode, acentos
  latinos plegados, CJK por carácter), n-gramas de caracteres opcionales y campos
  estructurados del JSON (`campo=valor`, números por orden de magnitud), con
  hashing trick con signo sobre FNV-1a + fmix64 (2^18 cubos por defecto).
- Modelo: regresión logística multinomial o binaria (`-one-vs-rest`), AdaGrad con
  L2, pesos por clase y parada temprana; cuantizado a int16 con escala por clase
  (concordancia con el float64: 100 % en la evaluación). Determinista con la
  semilla, también entre arquitecturas.
- Cada predicción trae etiqueta, probabilidad calibrada (temperatura), umbral τ de
  su clase elegido para una precisión objetivo (0,95), decisión `confident` /
  `escalate` y, si se pide, la evidencia (características por w·x): lo que
  necesita la cascada Chispa → VON.
- Inferencia entera y sin FMA: mismos bits en amd64 y arm64. 1,5 µs por texto de
  200 caracteres (6 µs con n-gramas) en un M4, sin reservas, segura en
  concurrencia.
- Fichero `.chispa`: magia, versión, especificación y su hash, pesos densos o
  dispersos (1,1 MB para 10 clases), CRC-32C; el cargador acota lecturas y
  reservas y rechaza ficheros corruptos sin pánico (`FuzzLoad`).
- Evaluación real con 4 304 commits convencionales de repos locales
  (`tools/chispa-commits`, `scripts/chispa-eval.sh`): exactitud 0,64–0,67 frente a 0,55
  de unas reglas y 0,32 de la clase mayoritaria; pero la precisión prometida por
  los umbrales cae de 0,95 a 0,77–0,85 con el cambio temporal y a 0,58 entre
  repos. Opt-in hasta que cada tarea tenga su evaluación.

### Gateway de IA (`kling ai`)

Diseño, API y cifras en [docs/ai-gateway.md](docs/ai-gateway.md).

- **`kling ai serve`**: Chispa y VON detrás de una API. Chispa clasifica, enruta y
  filtra dentro del proceso (`POST /v1/classify`, `/v1/decide`; cuando duda,
  contesta con `escalate: true` y quien llama decide); VON genera
  (`POST /v1/generate` con plantilla por tarea, y `/v1/chat/completions`,
  `/v1/completions`, `/v1/models` compatibles con OpenAI, con streaming). Registro
  de modelos y tareas en `~/.config/kling/ai.json`; `SIGHUP` o `kling ai reload` lo
  releen sin cortar nada.
- **La cascada Chispa → VON solo con pruebas**: `escalate_to` en una tarea se activa
  únicamente si `kling ai eval <tarea> -data test.jsonl` muestra, con datos
  etiquetados de la tarea, que gana a Chispa solo (McNemar exacta, p < 0,05) con los
  mismos modelos (sha256 del `.chispa`, dorado) y ajustes que se sirven; el registro
  se guarda en `ai-evals/<tarea>.json`. Si no, el gateway la rechaza y dice por
  qué, salvo `escalate_force`. Con la cascada apagada nunca se llama a VON a
  escondidas. `-von-alone` mide también a VON solo.
- Medido en commits (861 de prueba): Chispa solo 0,640; cascadas con Qwen2.5 0.5B
  0,429, 1.5B 0,520 (Q4_K_M) / 0,498 (Q8_0), 3B 0,540: la puerta las rechaza todas.
- Escala a cero con `pkg/scheduler`: réplicas `gw-<dorado>-*` con la etiqueta
  `ai.gateway=<id>`, thaw al llegar, freeze al quedarse ociosas, réplicas por
  concurrencia con tope por modelo, `-keepwarm` por popularidad. En un Mac, una
  generación caliente contesta en 9 ms y una réplica congelada en ~1,5 s.
- `kling ai calibrate`: recalibra los umbrales de Chispa con lo que VON contestó en lo
  escalado y en auditorías (`audit`), con mitad de evaluación; solo escribe si
  mejora, y se niega si Chispa dejaría de contestar. Recalibrar invalida la
  evaluación de la cascada.
- Seguridad: socket Unix 0600 por defecto; TCP solo con `-listen` y token
  (fichero 0600 o `$KLING_AI_TOKEN`); tokens con nombre y cuota; rutas
  `/v1/admin/*` solo con el token principal; cuerpos acotados y JSON estricto; el
  invitado no es de fiar (plazos, topes de respuesta, sin reenviar cabeceras ni
  rutas de control); semilla del host por petición.
- `/metrics` en formato Prometheus: cobertura y escalado de Chispa, latencias por
  fuente, thaws y arranques en frío por modelo, réplicas por estado.
- `pkg/scheduler`: puerto del invitado configurable, etiquetas y prefijo propios
  (solo adopta lo suyo), `MachineTTL` (con la capacidad `renew`, un arrendamiento
  que se renueva también antes del thaw de un scale-out), el scale-out reutiliza
  réplicas congeladas en vez de dejarlas en disco, marca contra adopciones
  dobles, tope de réplicas por servicio y `OnAcquire` para medir los arranques.

### Despliegue: la unidad de systemd ya no lleva valores de un host concreto

`packaging/kling.service` traía grabado `KLING_SOCKET_USER=juan`: en cualquier
otro host, `make deploy` volvía a instalar la unidad y pisaba en silencio lo
que ese host hubiera configurado, dejando el CLI sin acceso al socket (pasó en
el laboratorio). Ahora la unidad no lleva ningún valor propio de una máquina:
los lee de `EnvironmentFile=-/etc/default/kling` (opcional), y `make deploy`
crea ese fichero solo la primera vez, con `KLING_SOCKET_USER` a partir del
usuario de `HOST`; en los redespliegues siguientes no lo toca.

### Arreglos

- **El daemon ya no congela una instancia del gateway a media petición.** Las
  instancias nacen con TTL 2×idle como red de seguridad, pero el daemon lo cuenta
  desde la creación y ni `thaw` ni el tráfico HTTP lo reinician. Una instancia
  creada hace más de 2×idle, congelada por ociosa y despertada por una petición,
  volvía a congelarse en ~10 s con la petición en curso; y una que atendía sin
  parar se congelaba al cumplir 2×idle. Ahora el planificador renueva el TTL
  antes de despertarla, al adoptarla y en cada vuelta del segador, con la ruta
  nueva `POST /machines/{ref}/renew` (capacidad `renew`). Un sandbox sigue sin
  renovarse al despertar y no se puede renovar por esa ruta. Contra un daemon
  anterior el planificador se comporta como antes.
- **El gateway de IA ya no agota los puertos efímeros del host.** Cada decisión
  contra una réplica (Chispa `backend: microvm`, VON, codificadores) abría una
  conexión TCP nueva; un conjunto de CI entero dejaba decenas de miles en
  `TIME_WAIT` y en el Mac acababa en `can't assign requested address` y 503.
  Ahora hay un transporte con keep-alive y tope (64) por réplica; sus
  conexiones se cierran al congelarla o pausarla (`Scheduler.OnSleep`) y tras
  despertarla, y una reutilizada que muere sin respuesta se repite una vez.
  Contra una réplica falsa: de ~30 000 a ~85 000 decisiones/s y de una conexión
  por decisión a 8 (60 000 seguidas ya no fallan).
- **`max_replicas` es un tope duro.** Bajo concurrencia, cada petición veía
  menos réplicas que el tope y creaba la suya: con `max_replicas: 2`, 8
  réplicas. Ahora el scale-out reserva la plaza con el candado tomado y cuenta
  las que están naciendo; lo que no cabe se reparte entre las que hay, y si
  ninguna puede atender, `scheduler.ErrMaxReplicas` (503 en el gateway).
- **Encoger una capa ya no la corrompe.** `81-base-image.sh` (constructores
  base, llm y chispa) creaba la capa con `resize_inode`, y `resize2fs -M` dejaba
  las capas pequeñas y casi llenas (toda capa de Chispa, por debajo de ~60 MiB)
  con «Resize inode not valid» en e2fsprogs 1.47.0: el constructor chispa
  fallaba. Ahora la capa nace sin `resize_inode` (solo sirve para crecer
  montada), se encoge una copia que solo se acepta si `e2fsck` no encuentra
  nada (su código de salida con `-n` no basta: 1.47.0 da 0 tras contestar «no»),
  y si no se puede encoger queda entera y sana.
- El mismo riesgo estaba en `70-build-minimal-image.sh` y
  `71-build-glibc-base.sh`: creaban la imagen base con `resize_inode` y le
  hacían `resize2fs -M` directo, sin comprobar el resultado. Ahora usan el
  mismo encogido seguro que `81-base-image.sh`, factorizado en
  `scripts/lib-ext4-shrink.sh`.
- Tests: los de `GET /images` del daemon cerraban mal su `Manager`, y la
  escritura de estado pendiente caía sobre el TempDir mientras se borraba.

## v0.10.0 — 2026-09-23

### Carpetas compartidas

`kling run -share SRC:DST[:copy|ro|rw]` (repetible; también en `kling sandbox
create`, en `POST /machines` y en `POST /sandboxes`). Diseño, límites y modelo de
amenaza en [docs/compartir.md](docs/compartir.md).

- **copy** (por defecto): el CLI empaqueta la carpeta local en un tar y la sube
  (`POST /shares/uploads`, también por SSH); el daemon valida cada entrada
  (nada de rutas absolutas, `..`, enlaces que salgan o atraviesen otros, enlaces
  duros, dispositivos ni FIFOs; tamaño acotado por `daemon.share_copy_max_mib`,
  1 GiB por defecto), construye un ext4 de solo lectura con `mke2fs -d` y lo
  engancha como un volumen de solo lectura más. Lo monta hasta un agente
  anterior.
- **ro / rw**: la carpeta del host del daemon, en vivo. El agente de invitado
  habla él mismo el protocolo FUSE del kernel (sin libfuse ni cgo) y pide cada
  operación por ruta al daemon por una conexión que abre el daemon
  (`POST /share/attach`, `Upgrade: kling-share/1`); el daemon la sirve con
  `os.Root`, así que ni `..` ni un enlace sacan nada de la carpeta. `ro` lo impone
  el daemon (`EROFS`). Sin enlaces simbólicos, duros ni nodos nuevos; todo es de
  root para el invitado y lo que crea se entrega al dueño de la carpeta.
  Funciona con `egress none`, sobrevive a `freeze`/`thaw` (los ficheros abiertos
  se reabren solos) y a reiniciar el daemon. Solo carpetas bajo
  `daemon.share_roots` (o `KLING_SHARE_ROOTS`), vacío por defecto.
- `commit` de una máquina con carpetas: `409`. `run -from` con carpetas: `400`.
  Una imagen sin agente, o con uno anterior, lo dice claro.
- `kling ps` enseña la columna `SHARES` si alguna máquina tiene; `kling inspect
  <ref>` nuevo, con el estado de cada carpeta viva; `kling info` dice las raíces
  permitidas. Capacidades `shares-copy` y `shares-live`.
- Medido en el laboratorio (Firecracker anidado en un Mac, arm64): lectura y
  escritura secuencial ~12–18 MB/s (el techo es el limitador de 16 MiB/s de la
  red del invitado), ~200–250 creaciones/s y ~1250 `stat`/s de ficheros pequeños;
  `npm install express` en la carpeta, 56 s frente a 45 s en el disco de la
  máquina.

### Arreglos

- **El daemon de systemd lee la configuración de root.** Sin `$HOME` (un
  servicio sin `User=`), la ruta de la configuración salía relativa a `/` y el
  daemon no veía lo que `sudo kling config set` escribía en `/root/.config`.
  Ahora se busca el directorio del usuario en la base de usuarios.

## v0.9.1 — 2026-09-23

### Reloj y entropía propios tras restaurar

- **`POST /resync` en el agente de invitado** (`pkg/guest`, así que lo tienen
  `kling-guest` y el puente de kindling-mcp en cuanto se recompilan): recibe la
  hora del host y entropía fresca, pone el reloj de pared y mezcla la entropía
  acreditándola y forzando la resiembra del CRNG (`RNDADDENTROPY` +
  `RNDRESEEDCRNG`). Cuerpo acotado y validado.
- **El daemon lo llama tras cada restauración** —`thaw` y `run -from`, en los
  dos backends— antes de dar la máquina por arrancada. En macOS, donde
  Virtualization.framework no tiene VMGenID, dos réplicas del mismo snapshot
  sacaban los mismos aleatorios (ids de sesión MCP idénticos) y el reloj se
  quedaba en la hora del volcado; en Linux VMGenID ya resembraba, pero el reloj
  también se quedaba parado. Un agente anterior o una máquina sin agente no
  hacen fallar la restauración: se avisa una vez por imagen. Capacidad
  `guest-resync`; el evento de thaw dice cuánto costó.
- `guest.IsControlPath`: las rutas del agente que solo debe usar el host, para
  que un proxy que reenvía peticiones de terceros (el gateway MCP) las corte.
- Medido: 1–2 ms por restauración en macOS. En Firecracker (laboratorio anidado)
  el resync es la primera petición al invitado restaurado y se lleva los
  ~150–250 ms que antes pagaba el primer cliente; la siguiente va como siempre.
  Para que una máquina sin agente no pague segundos en cada restauración,
  `freeze` sondea el puerto del agente antes de pausar (el `thaw` no lo intenta
  si nadie escuchaba) y un snapshot sin agente se recuerda 10 minutos.

### Arreglos

- **Tras reiniciar el daemon, las máquinas en jail se readoptan con el socket
  de su chroot.** Se readoptaban con el de su directorio, que no existe:
  seguían corriendo, pero `freeze`, `stop` y el resto de llamadas a su VMM
  fallaban con `dial unix .../fc.sock: no such file or directory` hasta
  destruirlas. Lo mismo al descongelar una máquina que ya corría.

### Planificador

- **`Bind` ya no pisa una sesión fijada a otra instancia.** El mapa de rutas
  está indexado por la clave de sesión, y con el id del invitado como clave dos
  clientes que recibían el mismo id acababan en la misma ruta: el segundo
  reapuntaba en silencio la sesión del primero a su microVM. Ahora se niega y lo
  registra.
- **`BindGuest(clave, idInvitado, ...)`**, `Route.GuestSID()`,
  `NewSessionKey()` y `ErrSessionTaken`: quien enruta acuña su propia clave
  (128 bits de `crypto/rand`) y guarda aparte el id del invitado para traducir
  entre los dos. La API anterior sigue compilando.

## v0.9.0 — 2026-09-23

kindling corre nativo en macOS: el daemon, en un Mac con Apple Silicon, arranca
las microVMs con Virtualization.framework en vez de Firecracker. Ver
[docs/mac.md](docs/mac.md) y el contrato con el ayudante en
[docs/backend-vz.md](docs/backend-vz.md).

### macOS nativo (backend `vz`)

- **Un `kling-vz` por microVM** que habla el mismo API que Firecracker: el
  ciclo de vida, los snapshots dorados, el TTL, los sandboxes, el exec y el
  planificador funcionan sin reescribirse. Lo que en Linux hace el host
  alrededor del VMM —namespace, iptables, cgroups, jailer, `/proc`— va por
  etiquetas de compilación: la red y los puertos se le piden al ayudante, el
  overlay se clona con `clonefile`, la admisión mira `kern.memorystatus_level`,
  los huérfanos se buscan con `ps` y la memoria de cada VMM con `GET /kling/stats`.
- **El backend es una clave de configuración**: `kling config set daemon.vmm vz`
  (o `firecracker`); vacía, el de la plataforma. Se valida contra la máquina y
  `KLING_VMM` la sustituye con un nombre o una ruta. `kling info` dice el
  `backend` y la `arch` del daemon.
- **Sin root**: raíz en `~/Library/Application Support/kindling` y socket dentro;
  `kling` sin `-H` lo encuentra. `kling up` diagnostica Apple Silicon, macOS 14+,
  `kling-vz` firmado, e2fsprogs de Homebrew y las imágenes, y arranca el agente
  de launchd si está instalado.
- `kling-vz` vive en `vz/` como módulo aparte (`make vz`): el `go.mod` de la raíz
  sigue sin dependencias ni cgo.
- Límites frente a Linux: ~350 MiB por restauración (no se comparte la memoria
  del dorado), sin construcción de imágenes, sin techo de CPU ni jailer, 4
  arranques simultáneos por defecto y `squeeze` a ciegas (el framework no da
  estadísticas del invitado).

### Imágenes entre daemons

- **`GET/PUT /images/{name}/blob`** (capacidad `image-blobs`): la imagen, la
  capa, la receta y el kernel, en flujo, con sha256 verificado y renombrado
  atómico. Nunca sustituye una imagen en uso por un contenido distinto.
- **`kling images copy <name> -from <host> [-to <host>]`** mueve una imagen de un
  daemon a otro con todo lo que necesita para arrancar (kernel, base de una
  imagen por capas, receta); lo que el destino ya tiene idéntico no se manda, y
  se niega si las arquitecturas no coinciden. Es como se consiguen imágenes en
  un Mac: en macOS `POST /images` contesta 501.

### Direcciones de los invitados

- **`Machine.Forwards` y `Machine.Addr(port)`**: en macOS todos los invitados
  tienen la misma IP y se alcanzan por puertos de loopback que abre su ayudante.
  El proxy del daemon, exec, shell, los volúmenes y `pkg/scheduler` resuelven
  la dirección con `Addr`, que en Linux sigue siendo `IP:puerto`.
- `pkg/scheduler`: `Instance`, `Route` y `Warm` ganan `Addr(port)`; hay
  `AliveAddr`, `WaitReadyAddr` y el gancho `PrepareAddr`. `IP()`, `Alive`,
  `WaitReady` y `Prepare` se conservan: kindling-mcp compila igual y migra aparte.

## v0.8.0 — 2026-09-23

Cierra lo que quedaba abierto de estabilidad, elasticidad y seguridad tras v0.7.

### Estabilidad

- **Un volcado de congelación a medias ya no se da por bueno.** Firecracker
  escribe `snap.file` y `mem.file` sin temporal; si el daemon moría a mitad, al
  volver la máquina figuraba congelada y el thaw cargaba un volcado truncado.
  Ahora cada congelación deja una marca al empezar y un sello al terminar, con el
  sha256 del estado y el tamaño de la memoria, y reconcile y thaw lo exigen.
- **Los restos de un commit interrumpido se pueden borrar**, y el vigilante los
  recoge solo; antes bloqueaban hasta `commit -replace` del mismo nombre.
- **Borrar directorios huérfanos ya no congela el daemon**: bajo el cerrojo solo
  se mueven a una papelera, y el borrado de GiB se hace fuera.
- **Congelar dentro de jailer reanuda la máquina** si falla recuperar el volcado,
  en vez de dejarla en pausa figurando como en marcha.
- **Una máquina recién creada no se pierde si el daemon muere justo después**:
  su registro se escribe a disco antes de lanzar su VMM, y las escrituras de
  estado se serializan con una generación que descarta fotos viejas.

### Elasticidad

- **Memoria elástica**: `kling run -mem-max N` arranca con un techo y el globo
  retiene la diferencia; `kling resize <ref> -mem M` la sube o la baja en caliente.
- **Admisión por presión real**: con PSI por encima del 20 %, 507 aunque
  MemAvailable parezca holgado; con poco disco, 503 (y no 507, para que nadie
  congele para "hacer sitio" escribiendo más en disco).
- **Escalado por carga** en `pkg/scheduler` (`MaxInflight`, `MaxReplicas`): una
  instancia saturada de llamadas en vuelo ya no recibe sesiones nuevas aunque le
  quepan.
- **Puerta de arranque según el host**: 2 arranques a la vez en un host anidado,
  la mitad de los núcleos (hasta 8) en hierro desnudo.

### Seguridad

- **Snapshots firmados** con una clave del host (HMAC-SHA256): detecta
  manipulación y snapshots traídos de otro host. `KLING_REQUIRE_SIGNED=1` rechaza
  los anteriores, que no llevan firma.
- **El proxy al invitado solo llega al puerto del agente** salvo los declarados
  en la etiqueta `kling.ports`.
- **Jailer por defecto cuando está instalado**; `KLING_JAILER=0` lo apaga. Activarlo
  destapó un fallo que llevaba ahí desde que existe el modo jailer: descongelar una
  máquina de imagen por capas enlazaba en la jaula una ruta monolítica que no existe.
  Corregido, y el e2e ahora congela y despierta una imagen por capas.
- `kling info` dice si `$KLING_ROOT` está cifrado en reposo; receta en
  [`docs/cifrado.md`](docs/cifrado.md).

### Otros

- `kling volume populate` usa la ruta de ejecución en streaming, con la antigua
  como respaldo para imágenes anteriores a v0.7.

## v0.7.0 — 2026-09-23

**Sandboxes para agentes de código.** El núcleo gana lo que necesita un agente para
ejecutar lo que escribe sin tocar el host: exec en streaming, ficheros y sandboxes de
usar y tirar. Guía en [`docs/exec-sandbox.md`](docs/exec-sandbox.md).

| kindling | kindling-mcp |
|---|---|
| v0.7.x | v0.1.x |

### Novedades

- **`kling shell <ref>`**: una terminal interactiva dentro de la microVM, con
  pseudoterminal de verdad, redimensionado y Ctrl-C interrumpiendo lo de dentro y
  no la sesión. Es un cambio de protocolo (`Upgrade: kling-shell/1`) con tramas en
  los dos sentidos; el daemon valida cada una en vez de reenviar a ciegas. El
  agente monta `devpts` si falta, así que no hay que reconstruir imágenes.
- **`kling sandbox create|ls|renew|rm`** (`/sandboxes`): una microVM con exec, sin red
  por defecto, que se destruye al vencer su TTL (10 min por defecto). Desde un snapshot
  con exec arranca en ~300 ms con el estado de la plantilla.
- **`kling exec`** (`POST /machines/{ref}/exec`): stdout y stderr por separado y en
  streaming (NDJSON, también por SSH), stdin, entorno, directorio, plazo que mata al
  grupo de procesos (código 137) y topes de salida por flujo. Termina con el código del
  comando remoto. `?wait=1` da el resultado agregado.
- **`kling cp`** (`/machines/{ref}/files`): subir y bajar ficheros, con escritura
  atómica y sin seguir enlaces en el último componente.
- **`-on-ttl freeze` en los sandboxes**: en vez de destruirse al vencer, se
  duermen a coste cero y el siguiente exec los despierta en milisegundos. Con
  `freeze` el TTL cuenta inactividad; con `remove`, vida máxima.
- **`kling run -allow-exec` y `-on-ttl remove`.** `allow_exec` viaja por el API como
  opt-in explícito, se graba en el snapshot con `commit` y las instancias lo heredan;
  pedirlo sobre un snapshot sin él es `409`.
- El agente de invitado (`pkg/guest`, `kling-guest`) sirve `/exec/stream` y `/files`,
  solo con `kling.exec=1`.
- `GET /info` anuncia las capacidades `exec` y `sandboxes`.

### Correcciones de robustez

- **Un fallo posterior al arranque ya no deja un firecracker vivo.** Restaurar
  desde un snapshot y descongelar no mataban el proceso que acababan de lanzar
  —el arranque en frío sí lo hacía—, así que cada intento fallido (el caso real
  es el TSC invalidado tras reiniciar el host) retenía su RAM para siempre,
  invisible para `kling ps`. Descongelar, además, dejaba la red montada y la
  máquina figurando como congelada, y el siguiente intento readoptaba ese proceso
  vacío como sano.
- **Los VMM huérfanos se recogen en marcha**, no solo al arrancar el daemon.
- **El TTL no se reinicia al despertar**: se cuenta desde su propio reloj
  (`ttl_at`), no desde el último arranque. Un sandbox que dormía y despertaba
  podía no vencer nunca.
- **Una máquina con un secreto inyectado ya no reintenta congelarse cada 10 s
  para siempre**: se retira su TTL, una vez y diciéndolo en el log.
- **Antes de rechazar por memoria se pide prestado a los globos** de los
  invitados vivos, que devuelven lo que no usan sin congelar a nadie; y el
  desalojo puede soltar una instancia precalentada, que hasta ahora nunca ocurría.
- **El tope de máquinas del daemon se distingue de la falta de memoria** (409
  propio) y se puede subir con `KLING_MAX_MACHINES`.

### Cambios que se notan

- Las imágenes construidas antes de v0.7 llevan un agente sin exec en streaming: el
  daemon contesta `501` y pide reconstruirlas (`kling images toolchain`, o
  `kling images build -builder base`). `kling volume populate` sigue funcionando con
  ellas.
- `scripts/90-e2e.sh` prueba exec, ficheros y sandboxes, y compara con los mensajes en
  inglés del CLI.

## v0.6.0 — 2026-09-23

**El núcleo deja de llevar MCP.** Todo lo de alojar servidores MCP —el puente, el
gateway, el catálogo, `kling mcp`, `add`, `search`, `connect`, `export`, `memory`,
`migrate`— se muda a su propio repositorio y binario,
[kindling-mcp](https://github.com/juan52878911/kindling-mcp) v0.1.0. `kling` sigue
siendo el único comando: con kindling-mcp instalado, esos comandos aparecen en él
como antes, servidos por la extensión.

| kindling | kindling-mcp |
|---|---|
| v0.6.x | v0.1.x |

### Para actualizar

1. Actualiza kindling (`make deploy` o el instalador) y después instala
   kindling-mcp en tu máquina y en el host del daemon (su `make deploy`), que
   instala el puente, el empaquetador, el constructor `mcp` y las unidades
   `kling-gateway` y `kling-heal`, ahora con `ExecStart=/usr/local/bin/kling-mcp`.
2. Nada que migrar a mano: el daemon pasa catálogos, salud y links de v0.4 a
   anotaciones y store, y `kling` mueve la sección `memory` de la configuración a
   `extensions.mcp` (`kling config set mcp.memory.enabled true`).

### Novedades

- **Constructor `base` en el núcleo** (`scripts/81-base-image.sh`): una capa con
  paquetes de apk/apt y `kling-guest` como PID 1. `kling images toolchain` lo usa.
- **Unidades de extensiones en `kling up`**: el manifiesto declara `units` y
  `kling up` las arranca con el daemon si están instaladas.

### Cambios incompatibles

- Fuera del núcleo los comandos MCP y `kling-bridge`; `install.sh --bridge` avisa
  de que el puente viene con kindling-mcp.
- Retiradas las rutas deprecadas en v0.5 (`/snapshots/{n}/catalog`,
  `/snapshots/{n}/health`, `/links`, `/images/refresh-bridge`,
  `/images/{n}/capabilities`) y los campos `tools`/`health*` del snapshot. Ver
  [`docs/api.md`](docs/api.md#rutas-retiradas-en-v06).
- `POST /images` exige `builder`; el proxy al invitado exige `path` (salvo
  `probe_only`).
- `kling images refresh` pasa a ser `kling mcp refresh-bridge`.
- Fuera de `pkg/api` los tipos de MCP (`ToolSpec`, `Link`, `Capabilities`...);
  viven en kindling-mcp.

## v0.5.0 — 2026-09-23

kindling se separa en dos: **el núcleo de microVMs** y **kindling-mcp**, lo que se
venía usando para alojar servidores MCP. Esta versión hace la separación dentro del
repositorio sin que cambie nada de lo que se teclea: `kling mcp import`, `kling add`,
`kling connect` y el gateway funcionan igual, ahora servidos por una extensión
incorporada. El siguiente paso mueve kindling-mcp a su propio repositorio y binario.
Ver [`docs/extensions.md`](docs/extensions.md) y [`docs/api.md`](docs/api.md).

### Novedades

- **Extensiones de `kling`.** Un ejecutable `kling-<nombre>` en el `PATH` (o en
  `$KLING_PLUGIN_PATH`) añade subcomandos a `kling` declarándolos en un manifiesto:
  aparecen en la ayuda y en el completado, y `kling` les pasa el control con `exec`,
  así que códigos de salida y señales llegan intactos. Pueden añadir líneas a
  `kling status` y claves a `kling config`. `kling plugins` las lista. Los comandos
  MCP pasan por este mismo camino como extensión incorporada.
- **Anotaciones de snapshot y store en el daemon**, para que una extensión guarde
  su estado sin que el núcleo lo entienda. El catálogo y la salud de MCP son ahora
  las anotaciones `mcp.tools` y `mcp.health`; los servidores externos enlazados,
  `store/mcp/links`.
- **Constructores de imágenes con nombre.** `POST /images` con `builder` ejecuta un
  constructor de root instalado por el administrador en
  `/usr/local/lib/kindling/builders/`; `kling images build <nombre> -builder <b>`.
- **Ficheros dentro de imágenes**: `kling images cat` e `images put` leen o ponen al
  día un fichero de una imagen ya construida, sin reconstruirla.
- **`kling-guest`**, el agente de invitado genérico (exec, volúmenes, MMDS, DNS)
  para microVMs sin servidor MCP. `kling-bridge` lo embebe.
- **Paquetes públicos para extensiones**: `pkg/api`, `pkg/config`, `pkg/guest`,
  `pkg/scheduler` (planificación genérica del gateway), `pkg/plugin`,
  `pkg/transport`, `pkg/durable`, `pkg/panico`.
- **`GET /info` devuelve la versión real del daemon** (era siempre `0.1.0`) y la
  lista de capacidades del API.

### Cambios que se notan

- **`kling status -json`**: lo del gateway y los agentes pasa de `gateway` y
  `agents` a `extensions.mcp.gateway` y `extensions.mcp.agents`.
- **El proxy al invitado ya no asume MCP.** Quien pasa `path` recibe solo lo que
  pide. Las peticiones sin `path` (clientes v0.4) conservan los valores de antes
  hasta v0.6.
- Las rutas `/snapshots/{n}/catalog`, `/snapshots/{n}/health`, `/links`,
  `/images/refresh-bridge` y `/images/{n}/capabilities` quedan como alias
  deprecados; se retiran en v0.6. Al arrancar, el daemon migra `links.json` al
  store y deja el original como `links.json.migrated`.

### Correcciones

- **`-bundle` copia el `package.json` junto al bundle.** `server-sequential-thinking`
  lee su versión del `package.json` al arrancar, buscándolo junto al fichero que
  ejecuta; el bundle de esbuild vivía solo en `/opt` y el proceso moría con
  "Could not locate package.json for server version", que el gateway devolvía
  como 502 en el `initialize`. Ahora `scripts/80-mcp-image.sh` deja en
  `/opt/package.json` el del paquete que aporta el entry (el mismo que encontraría
  sin empaquetar). De los servidores oficiales de npm sólo éste lo hace; el SDK
  de TypeScript no.

## v0.4.0 — 2026-08-28

Ochenta y un commits desde v0.3.0. La versión va de **que funcione** a **que se
recupere solo y que no se caiga entera**: casi todo lo de abajo salió de mirar el
sistema vivo, no de leer código.

El fallo que mejor resume la tanda: `semgrep` y `playwright` estuvieron **297 horas
caídos** y nadie se enteró, porque la salud sólo se registraba cuando llegaba una
petición. Un servicio que nadie llama se queda roto en silencio hasta que alguien lo
llama.

### Novedades

- **`kling mcp heal`** con temporizador de systemd (`OnBootSec=2min`,
  `OnUnitActiveSec=6h`). Un reinicio del anfitrión invalida **todos** los dorados a
  la vez —Firecracker los ata a la frecuencia del TSC— y hasta ahora había que
  reimportarlos a mano. `heal` sondea, y sólo reconstruye lo que el TSC invalidó:
  un servicio enfermo por otra causa no se arregla rehaciéndolo, y reimportarlo
  sería ruido que tapa el problema real. Reconstruye con la configuración
  **original** —memoria, vCPUs, egress, volúmenes, etiquetas— no con la de por
  defecto.

- **`kling mcp verify` puede fallar.** Antes salía 0 sin ejercitar nada: pedía
  `tools/list` y se daba por satisfecho. Ahora llama a una herramienta de verdad
  (`browser_navigate` sobre `about:blank` en las imágenes de navegador) y consulta
  `/dns` del puente, que devuelve los nameservers del invitado y si resuelve.

- **`kling images rm`**, que se niega si la imagen es base de otra capa, la usa un
  dorado o tiene una máquina viva.

- **Base glibc con `chrome-headless-shell`** (`scripts/71-build-glibc-base.sh`).
  Medido contra el Chromium de Alpine sobre tres sitios reales: **misma cantidad de
  texto extraído**, 637 MB frente a 986,8 MB y 116 ms frente a 401 ms. Chromium se
  queda como opción; el puente no sabe qué motor arranca, lee
  `/etc/kling/browser.json`.

- **`images refresh` hace crecer la imagen** cuando el puente no cabe dentro, en vez
  de fallar. Y **graba la salud** en lugar de sólo imprimir un aviso: al refrescar
  invalida el dorado, y antes el servicio quedaba roto sin que nadie lo supiera.

### Correcciones — el 502 permanente

Tres fallos encadenados que se disfrazaban de uno solo, y que dejaban un servicio
devolviendo 502 para siempre:

- el recolector de basura medía **el sistema de ficheros entero**, así que se
  desataba por disco que no era suyo;
- el gateway **cacheaba la instancia muerta** y seguía marcándola hacia ella;
- la salud se anotaba **al adquirir** la instancia, no según el resultado, así que
  un servicio roto se reafirmaba sano en cada intento fallido.

Medido después: `memory` y `sequentialthinking` pasan de 502 a 200 en **688 ms**.

### Correcciones — seguridad

- **El gateway ya no reenvía su propio token.** Lo mandaba al invitado y a URLs de
  terceros: un servidor MCP comprometido se llevaba la credencial del agregador.
- **Las imágenes dejan de ser world-readable.** Contenían los ficheros `-env` con
  los secretos de cada servicio; ahora se hace `chown` al usuario del servicio con
  `0640`/`0750`.
- **`cpu` no es `cpuset`.** La detección de controladores de cgroup usaba
  `Contains`, y `cpuset` contiene `cpu` como subcadena: el límite se daba por puesto
  sin estarlo. Ahora se compara palabra a palabra.
- **Tests del cortafuegos de salida**: `isBlockedIP` cubre RFC1918, loopback,
  link-local —incluido el `169.254.169.254` de metadatos—, CGNAT, multicast y sus
  equivalentes IPv6; y `ParseEgress` **falla** ante un valor desconocido en vez de
  caer en el más permisivo.

### Correcciones — robustez

- **Los pánicos de los bucles de fondo quedan contenidos.** Había 21 goroutines y
  **cero** `recover()`: un nil-pointer en el reconciliador o en el persistidor de
  estado mataba el proceso y dejaba huérfanas todas las microVM. Se envuelve **cada
  iteración**, no el bucle: contener el bucle entero dejaría el daemon vivo sin
  reconciliar nada, que es peor porque no se nota.
- **Registro de cerrojos con contador de referencias.** El `sync.Map` de antes
  borraba la entrada mientras otra goroutine seguía esperándola. Medido rompiendo el
  código a propósito: **1.758 entradas dobles** en la sección crítica.
- **Volúmenes: comprobar y reservar bajo el mismo cerrojo.** Dos arranques
  simultáneos podían quedarse el mismo volumen exclusivo. `RemoveVolume` tenía la
  misma carrera entre la comprobación y el `os.Remove`.
- **Escritura durable en un solo sitio**: fichero temporal, `fsync` del fichero,
  `rename`, `fsync` del directorio. Antes sólo `state.json` hacía `fsync`;
  `links.json`, la configuración, `meta.json` y las recetas de imagen no.
- **Matar el grupo de procesos, no sólo el pid del hijo**, para que no queden nietos
  huérfanos.
- **Los topes de tamaño fallan en vez de truncar.** Un JSON cortado por la mitad no
  es un JSON pequeño: es ilegible, y el error decía otra cosa.
- **`kling commit` exige que el invitado SIRVA** antes de congelar un dorado. Un
  snapshot tomado antes de tiempo restaura en 26 ms y luego no contesta, minutos u
  horas después, con un error que no menciona el commit.
- **`evictLRU` reponía la víctima** que no se pudo congelar, y prueba con otra en vez
  de rendirse.
- **`MCPPayload` elegía el primer evento SSE**, que puede ser una notificación; ahora
  busca la respuesta.
- **`callLink` reintentaba ante cualquier error**, incluidos los tiempos de espera;
  ahora sólo ante sesión caducada.
- Dos deref nil en `Freeze`/`Thaw`, `Stop` sin el cerrojo de ciclo de vida, `Remove`
  borrando su entrada demasiado pronto, y un firecracker huérfano al hacer `Thaw`.

### Rendimiento

- **El despertar baja de 4.350 ms a 175–202 ms**, y una carga de 20 peticiones de
  44,08 s a 4,66 s. El puente deja un hijo **caliente sin ligar**, así que el dorado
  no paga el arranque del runtime al restaurar.
- **El veredicto de integridad del snapshot se recuerda** en vez de rehashear 512 MiB
  en cada uso.

### Tests

Los cinco paquetes que no tenían ninguno: `internal/report`, `internal/assets`,
`internal/config`, `internal/events` y `cmd/notas-server`. Más los del registro de
cerrojos, la retirada de imágenes, el transporte, el cliente de Firecracker y el
cortafuegos. Cada arreglo se verificó **rompiendo el código a propósito** y
comprobando que el test se pone rojo.

### Actualizar desde v0.3.0

Un cambio de comportamiento, de la auditoría de entorno de más abajo: el puente
escucha en `127.0.0.1:9100` en vez de `0.0.0.0:9100`. Si el gateway corre en otra
máquina, hace falta `-listen 0.0.0.0:9100` explícito.

Instalar el temporizador de autocuración:

```sh
sudo cp packaging/kling-heal.service packaging/kling-heal.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kling-heal.timer
```

### Correcciones — auditoría de supuestos del entorno

Siete fallos que **no daban ningún error**: cada uno reportaba éxito mientras no
hacía lo que decía. Todos verificados en ejecución, no sólo leídos.

- **`kling memory install-service` instalaba un LaunchAgent que no podía arrancar,
  y salía 0.** `bridgePath()` podía devolver la ruta *relativa* `./kling-bridge-local`,
  y el cwd de launchd es `/`: el job moría con `EX_CONFIG` (78), con stderr vacío y
  sin escribir el log. Ahora la ruta es siempre absoluta, y se consulta a launchd si
  el job vive de verdad — `launchctl load` devuelve 0 aunque nunca llegue a arrancar.
  Los dos códigos que no dicen nada por sí solos se traducen: **78** (ruta relativa o
  inexistente) y **126** (sin permiso, típico bajo `~/Documents`, `~/Desktop` o en un
  volumen externo por TCC).

- **El puente ya no se expone a la red por defecto.** `-listen` pasa de
  `0.0.0.0:9100` a `127.0.0.1:9100` en `memory enable` e `install-service`. Lo que se
  envuelve suele ser la memoria personal, el puente **no autentica** —ni puede
  hacerlo de forma útil, porque `kling mcp link` no manda cabeceras— y `/reset` queda
  accesible para cualquiera que alcance el puerto. Exponerlo sigue siendo posible y
  legítimo con el gateway en otra máquina, pero ahora es una decisión y avisa.
  *Cambia comportamiento:* si el gateway corre en otro host, hace falta
  `-listen 0.0.0.0:9100` explícito.

- **El daemon arrancaba «sano» en un host donde no podía hacer nada.** Sólo se
  comprobaban `ip` e `iptables`. Ahora también `firecracker`, `setpriv` y `mkfs.ext4`,
  y los nombra todos de golpe. `setpriv` era el peor: no lo cubría ninguna
  comprobación y fallaba por microVM en pleno arranque, sin relación visible con la
  causa.

- **`install.sh` podía aceptar un checksum sin verificar nada.** Con `sha256sum`
  ausente, `EXPECTED` y `ACTUAL` quedan vacíos y `"" != ""` es falso: la comprobación
  **pasa**. Añadidas las guardas de vacío en los dos bloques.

- **El puente se aceptaba por existir, no por ser ejecutable en el destino.**
  `[ -f ]` no dice nada del formato: un Mach-O de `make bridge-local` pasaba, se
  instalaba como PID 1 de una imagen x86-64, y la microVM reventaba con un pánico del
  kernel sin mencionar al puente. Ahora se comprueba el mágico `\x7fELF`.

- **`printf %q` es de bash y los entrypoints son `#!/bin/sh`.** En Alpine eso es
  busybox ash, y para un argumento con tabulador `%q` emite `$'x\ty'`, sintaxis que
  dash no entiende. Sustituido por entrecomillado POSIX con comilla simple.

- **Una ruta de socket demasiado larga daba `bind: invalid argument`**, sin mencionar
  ni la longitud ni el socket. El límite de `sun_path` son 104 bytes en macOS y 108 en
  Linux; ahora se dice.

## v0.3.0 — 2026-08-13

Notas completas, con tablas comparativas: [`docs/RELEASE-v0.3.0.md`](docs/RELEASE-v0.3.0.md).

La v0.2.0 hizo kindling instalable, autenticado y con estado. Esta lo hace denso, paralelo
y compartido, y estrena soporte (limitado) para Mac Apple Silicon.

### Novedades

- **Misma herramienta en paralelo.** El gateway crea **réplicas por servicio** bajo demanda
  desde el snapshot dorado (COW); varias sesiones concurrentes ya no las topa el cap de
  sesión del puente.
- **`kling migrate`.** Mueve un MCP a kindling **conservando el nombre de la entrada y de
  las herramientas** (endpoint per-servicio): las skills que lo usaban siguen funcionando
  sin reescribirse.
- **Secretos por sesión vía MMDS**, inyectados en la microVM viva; un snapshot congelado
  nunca lleva secretos dentro.
- **Egress allowlist de dominios** (tercer modo, fail-closed): solo salen los dominios
  declarados, con resolver dinámico DNS→ipset.
- **Cuotas por token/tenant** en el gateway (reparto justo).
- **Devolver la RAM**: `kling squeeze` (balloon) reclama la memoria disponible; `/metrics`
  y `kling top` (PSS) hacen visible el peso real, contando el `mem.file` compartido.
- **Modo proxy HTTP/SSE** en el puente: soporta MCP que no hablan stdio.
- **Auto-detección de capacidades** (navegador/internet/nativo) y Chromium compartido con
  contexto por sesión.
- **zram opt-in** en el host para densificar.
- **Mac Apple Silicon (arm64), compatibilidad limitada.** `make deploy-mac` y binarios
  `kling-darwin-arm64`/`kling-linux-arm64`. Requiere M3+ y virtualización anidada; el
  arranque en frío es ~16 s bajo KVM anidado (vs ~3 s en Linux nativo) y el paralelismo
  práctico ronda ~8 réplicas. Límites y receta en [`docs/mac-arm64.md`](docs/mac-arm64.md).

### Correcciones

- `mcp import` respeta `-cpus` y `defaults.mem_mib`.
- El puente **recicla la sesión más ociosa** al llegar al tope (reconexión limpia en
  servicios de 1 sesión); `-e KEY=VAL` para hornear env que apagan el phone-home (semgrep:
  124 s → ~10 s).
- GET sin sesión a un servicio devuelve **405, no 404** (clientes streamable-HTTP cargan
  el endpoint per-servicio).
- Segador/evict sin perder trabajo en vuelo, reconciliación de rutas pegajosas por vida,
  suelo de `MemFree` y cierre del TOCTOU de memoria. Jailer opt-in en frío/restauración,
  matando VMMs huérfanos. Integridad (sha256) y salud del catálogo.

### Actualizar desde v0.2.0

- Corre `kling images refresh`: el puente trae el modo proxy y la inyección de secretos.
- El paralelismo de la misma herramienta no pide configuración; ajusta las cuotas por
  tenant si repartes un mismo token.
- En Mac: necesitas M3+ y `nested virt` (ver [`docs/mac-arm64.md`](docs/mac-arm64.md)).

## v0.2.0 — 2026-08-11

Notas completas, con tablas comparativas: [`docs/RELEASE-v0.2.0.md`](docs/RELEASE-v0.2.0.md).

La v0.1.0 demostraba que la idea funciona: microVMs que descongelan en milisegundos y un
gateway que las despierta bajo demanda. Esta la hace instalable, autenticada y capaz de
guardar estado.

### Novedades

- **`kling up` y `kling status`.** Instalar deja de ser tres scripts a mano como root: el
  kernel y la imagen base van dentro del binario.
- **El gateway exige token.** Despertar un snapshot es ejecutar código, y el gateway es lo
  único que escucha en red. Se genera solo la primera vez.
- **7 clientes de IA** en `connect -install`, frente a 2.
- **Catálogo oficial**: `kling search` y `kling add` contra `registry.modelcontextprotocol.io`.
- **Volúmenes persistentes**, con journal, hasta cuatro por microVM y compartibles en solo
  lectura: un escritor exclusivo, o cuantos lectores hagan falta.
- **`kling volume populate`**: instala paquetes DENTRO de una microVM desechable en vez de
  como root en el anfitrión.
- **`kling images toolchain`**, **`kling images refresh`** y **`kling images recipe`**.
- **NODE_PATH y PYTHONPATH automáticos** apuntando a los volúmenes que traen paquetes.

### Correcciones

Nueve fallos que solo aparecen metiendo servicios de verdad, entre ellos: `mcp import`
ignoraba `defaults.mem_mib` (la causa real de los timeouts en paralelo que se achacaban al
gateway), los snapshots no guardaban ni su política de red ni su volumen, los volúmenes se
formateaban sin journal, y el segador del gateway congelaba microVMs con trabajo en vuelo.

### Actualizar desde v0.1.0

- El gateway pide token: cópialo con `kling config set gateway.token …`.
- Reimporta los servicios: sus snapshots no guardan la política de red.
- Corre `kling images refresh`: el puente vive dentro de cada imagen.

## v0.1.0 — 2026-08-08

Primera release con binarios distribuidos. Antes de esta versión, `kling` solo
estaba disponible vía `make install` (compilación local).

### Novedades

- **Instalación con una línea** desde releases: `curl -fsSL .../install.sh | sh`.
  Sin Go instalado, sin clonar el repo, sin sudo.
- **Releases multi-plataforma** vía GitHub Actions: binarios pre-compilados para
  Linux (amd64/arm64), macOS (amd64/arm64). El bridge se publica por separado
  porque va dentro de las microVMs (estático, musl-safe).
- **Verificación SHA256** antes de instalar: cada release incluye `SHA256SUMS`
  y el instalador aborta si el checksum no coincide.
- **CLI `--dry-run`** para previsualizar qué se descargará y dónde quedará.

### Arreglos desde la última versión funcional (HEAD)

- **Snapshots stateful ya no rompen los restores.** El daemon ahora expone
  `POST /reset` en el bridge, y `kling mcp import` lo invoca (o espera al
  auto-reset del wrapper HTTP) antes de hacer commit. Sin esto, los snapshots
  dorados se congelaban con el servidor ya inicializado, y al restaurar el
  puerto 8080 nunca abría o el handshake daba 400/406.
- **`everything` (HTTP nativo)** ya funciona end-to-end con el wrapper de
  auto-reset (`/var/run/kling-http-reset-done` persiste en el overlay).
- **`filesystem-mcp` (stdio + bridge)** ya funciona end-to-end: el bridge
  tiene el endpoint `/reset` y `mcpImport` lo invoca tras capturar el catálogo.
- **CLI actualizado detecta las nuevas respuestas del daemon** — antes, un CLI
  viejo podía mostrar mensajes de error engañosos.

### Limitaciones conocidas

- **Windows no soportado.** `internal/machine/manager.go` usa `syscall.Kill`,
  `Setsid`, `Stat_t` que son POSIX. Si necesitas Windows, abre un issue.
- **Solo se publica el CLI para macOS** — el daemon requiere KVM, que en macOS
  no existe fuera de máquinas virtuales con VT-x anidado (que es justamente
  cómo se mide aquí: Proxmox + KVM + Firecracker).
- **El bridge solo se publica para Linux.** Va dentro de microVMs Alpine (musl);
  en macOS no tiene sentido empaquetarlo.