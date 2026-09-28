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

El acceso remoto es **SSH y nada más**: `ssh host kling dial-stdio`. La autenticación es la
de SSH; kindling no inventa credenciales propias.

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

El resolver de `allowlist` está acotado: como máximo 32 consultas a la vez y ~200/s (ráfaga
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

En macOS (`kling-vz`) no hace falta nada de esto: `egress.IsBlockedIP` ya trata cualquier
dirección que no sea IPv4 como bloqueada, porque el invitado nunca ha tenido IPv6 en ese
backend.

### 4. Una microVM no puede degradar a las demás

- **Caudal acotado** por dispositivo: 128 MiB/s de disco y 16 MiB/s de red, con limitadores
  de Firecracker.
- **Techo de CPU por máquina** con su propio cgroup (`-cpu-pct`, 50% de un core por
  defecto): un invitado en bucle no se come el host.
- **Tope de máquinas** (`MaxMachines = 256`) para que un cliente comprometido no agote el host.
- **RAM fija** por microVM; el invitado no puede pedir más.
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
  (403 al resto), cambia el marcador por la clave en las cabeceras (también dentro
  de `Authorization: Basic`), en la query y en el cuerpo (en flujo, con una ventana del
  tamaño del marcador: sin límite de tamaño ni de longitud declarada), sale por HTTPS
  verificando el certificado con un dialer que no conecta a IPs privadas, no sigue
  redirecciones y sustituye la clave por el marcador en cabeceras y cuerpo de la
  respuesta —también sus formas escapadas (JSON `\/` y `\u00XX`, percent-encoding,
  entidades HTML) y cada valor de cabecera tal y como salió sustituido, que es lo que
  cierra el eco de un `Basic` (la clave dentro del base64)—. Una respuesta con una
  codificación que no puede inspeccionar (brotli, deflate) no se entrega: 502. Acotado:
  32 peticiones en vuelo, 10 MiB de cuerpo, 64 KiB de cabeceras, hasta 1 MiB retenido
  por petición. Plazos: 60 s hasta las cabeceras de la respuesta, 120 s de inactividad
  (cada byte en cualquier sentido los renueva, también el plazo de la conexión del
  invitado) y un techo de 15 min por petición: un stream largo de un LLM pasa, y un
  invitado que gotea bytes para retener una plaza no la retiene más de 15 min. Un corte
  aborta la conexión, así que el invitado ve un error y no una respuesta truncada que
  parezca completa.
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
  proxy solo atiende en allowlist (403 en otro modo aunque el invitado conecte a mano),
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
    perfil de sandbox (`kling-vz.sb`) sigue limitando qué ficheros y qué red toca, pero
    no protege la memoria del propio proceso.
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

### 8. Ejecutar comandos dentro es opt-in y se decide al arrancar

`kling exec`, `kling cp` y los sandboxes ejecutan y escriben dentro de una microVM.
Solo existen en máquinas creadas con `allow_exec`, que viaja en la línea de comandos
del kernel: la escribe el host, el invitado no puede concedérsela, y sin ella las
rutas `/exec` y `/files` del agente no están registradas. Se congela con la memoria,
así que un snapshot de servicio (sin ella) nunca da máquinas que ejecuten, y el
daemon se niega a crear un sandbox desde él. El daemon no se fía del flujo del
agente: recorta la salida a los topes y valida cada evento. Los sandboxes nacen sin
red y se destruyen al vencer su TTL.

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

## Lo que NO está resuelto

Se enumera a propósito, porque una lista de garantías sin sus límites es propaganda:

- **Jailer con salida explícita.** Desde este cambio es obligatorio por defecto en Linux
  (ver 11): arrancar sin él exige `KLING_JAILER=0` a propósito, y eso deja un aviso en el
  log. Quien lo pone y no lo lee sigue corriendo el VMM con el sistema de ficheros del
  host y los permisos de su usuario.
- **Cuota de disco por overlay, no por host.** Cada overlay son 512 MiB lógicos; la
  admisión por disco (ver 12) impide crear máquinas nuevas con el disco casi lleno, pero
  las que ya corren pueden seguir llenando los suyos.
- **El cifrado en reposo es cosa del disco, no de kindling.** `kling info` dice si
  `$KLING_ROOT` está sobre dm-crypt; si no, quien tenga el disco tiene la memoria de las
  microVMs. Receta en `docs/cifrado.md`.
- **El daemon confía en quien alcanza su socket.** No hay autorización por operación: si
  entras, puedes con todo.
- **El proxy al invitado no filtra la ruta.** Solo llega a los puertos permitidos (ver
  10), pero dentro de ellos a cualquier ruta. No es una escalada —quien llega al socket
  ya manda— pero conviene saberlo si algún día el socket se comparte.
- **Una credencial se puede usar, aunque no leer.** El proxy impide que el invitado lea
  la clave o la saque a otro dominio, no que la use contra el suyo: es un oráculo de
  ella. Lo acota la clave misma (restringida, de solo lectura, con límites de gasto en
  el proveedor). `-allow-request` acota el oráculo a unas rutas, pero no mira la query
  ni el cuerpo, y lo que no controla es cómo interpreta el proveedor la ruta que recibe:
  un servidor que trate `;` o `\` como separadores, o `..;` como `..`, ve otra ruta que
  el proxy. Contra eso, patrones exactos mejor que `/**`. La redacción del eco es defensa
  en profundidad y cubre las transformaciones habituales, no todas las imaginables. Esto
  vale igual en macOS: `PUT /kling/credentials` lleva `allow` y `kling-vz` aplica las
  mismas reglas antes de reenviar.
- **En macOS la clave vive en un proceso que el invitado alcanza por la red.** Es el
  `kling-vz` de su máquina, que corre como el usuario y procesa su tráfico (ver 7). Un
  fallo explotable en su pila de red, DNS o MMDS que antes daba un proceso sin claves
  daría ahora la de esa máquina (solo la de esa: cada máquina tiene su `kling-vz`). En
  Linux la clave nunca sale del daemon.
- **Los secretos por sesión de MMDS (`sessions[<id>]`) están retirados.** El id de
  sesión lo genera el puente DENTRO del invitado (PID 1, root) justo al lanzar el
  hijo para `initialize`. Como el bridge y el servidor MCP corre como root y lee el
  almacén MMDS completo, una sesión comprometerida vería los secretos de todas las
  otras. El almacén sigue aceptando el campo para atrás-compatibilidad, pero se ignora
  y se avisa si está presente. Alternativas seguras: (1) credencial de plantilla (una
  por servicio, entregada a cada réplica al nacer); (2) proxy de credenciales para
  aislar claves por dominio; (3) VM efímera por sesión si necesitas secretos por
  sesión reales.
- **El puente local (`kling-bridge-local`) no autentica.** Por eso desde v0.4.0 escucha
  en `127.0.0.1` por defecto; exponerlo a la red es una decisión explícita
  (`-listen 0.0.0.0:9100`) y avisa.
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
