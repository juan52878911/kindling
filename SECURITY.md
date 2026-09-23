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

### 4. Una microVM no puede degradar a las demás

- **Caudal acotado** por dispositivo: 128 MiB/s de disco y 16 MiB/s de red, con limitadores
  de Firecracker.
- **Techo de CPU por máquina** con su propio cgroup (`-cpu-pct`, 50% de un core por
  defecto): un invitado en bucle no se come el host.
- **Tope de máquinas** (`MaxMachines = 256`) para que un cliente comprometido no agote el host.
- **RAM fija** por microVM; el invitado no puede pedir más.
- En el gateway, **cuotas por token/tenant**: varios clientes sobre un mismo token se
  reparten la capacidad en vez de matarse de hambre.

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

- **Un snapshot congelado nunca lleva secretos dentro.** Los secretos se inyectan por
  sesión vía MMDS en la microVM **viva** (`kling mmds`), y una máquina que los ha
  recibido **ya no puede congelarse** — se impone, no se aconseja.
- **El gateway no reenvía su propio token** ni al invitado ni a URLs de terceros: un
  servidor MCP comprometido no se lleva la credencial del agregador.
- **Las imágenes no son world-readable.** Contienen los ficheros `-env` de cada
  servicio; van con `chown` al usuario del servicio y permisos `0640`/`0750`.
- **La integridad de un dorado se comprueba** (sha256 de sus ficheros) antes de
  instanciarlo; el veredicto se recuerda y se re-verifica si tamaño o fecha cambian.

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

### 11. Jailer por defecto cuando está instalado

Si el binario `jailer` está y el daemon corre como root, las microVMs corren
dentro de él sin configurar nada; `KLING_JAILER=0` lo apaga. Antes había que
pedirlo, y la barrera más fuerte quedaba apagada justo donde nadie había leído
esto.

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
    1024 ficheros abiertos; 16384 en todo el daemon.
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

- **Jailer solo si está instalado.** Desde v0.8 se usa por defecto cuando el binario
  existe (ver 11); en un host sin él, el VMM ve el sistema de ficheros del host con los
  permisos de su usuario.
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
- **El puente local (`kling-bridge-local`) no autentica.** Por eso desde v0.4.0 escucha
  en `127.0.0.1` por defecto; exponerlo a la red es una decisión explícita
  (`-listen 0.0.0.0:9100`) y avisa.

## Ante un incidente

Aislar sin destruir pruebas:

```sh
kling stop <ref>     # mata el VMM, conserva overlay y snapshot
kling ps -a          # qué había, con su IP y su política de salida
kling topo           # de qué snapshot salió cada instancia
```

Si se sospecha del snapshot dorado, revisar las instancias antes de borrarlo: `kling rmi` se
niega si quedan vivas.
