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
- **Tope de máquinas** (`MaxMachines = 256`) para que un cliente comprometido no agote el host.
- **RAM fija** por microVM; el invitado no puede pedir más.

### 5. Aleatoriedad

Las instancias que restauran del mismo snapshot **clonan el estado del generador de
aleatoriedad** del invitado. Es un problema documentado por AWS: dos herramientas podrían
generar las mismas claves TLS.

Mitigación: cada microVM lleva un **virtio-rng**, y el kernel invitado tiene
`CONFIG_VMGENID=y`, así que detecta que viene de un snapshot y vuelve a sembrar su pool.

> El dispositivo de entropía debe estar presente **antes** de congelar: tras cargar un
> snapshot ya no se pueden añadir dispositivos.

### 6. Validación de entradas

Los nombres de snapshot llegan por la URL y se usan para construir rutas. Se validan contra
`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$` en **todos** los caminos —crear, leer y borrar— y no
solo al crear: un `../../etc` saldría del directorio de datos.

## Lo que NO está resuelto

Se enumera a propósito, porque una lista de garantías sin sus límites es propaganda:

- **Sin chroot.** El VMM no tiene privilegios, pero sí ve el sistema de ficheros del host con
  los permisos de su usuario. `jailer` daría además chroot y cgroups; no está integrado.
- **Sin límite de CPU por máquina.** Firecracker acota la RAM y el caudal de E/S, pero un
  invitado puede consumir su vCPU al 100%. Faltan cgroups.
- **Cuota de disco blanda.** Cada overlay son 512 MiB lógicos; un invitado puede llenarlos.
  Con muchas máquinas eso llena el host.
- **Sin cifrado en reposo** de snapshots ni overlays. Quien tenga el disco del host tiene la
  memoria de las herramientas.
- **El daemon confía en quien alcanza su socket.** No hay autorización por operación: si
  entras, puedes con todo.
- **Los snapshots dorados no se verifican.** No hay firma ni checksum, así que quien pueda
  escribir en `snapshots/` decide qué se ejecuta.
- **El proxy al invitado no filtra el destino.** `POST /machines/{ref}/guest` acepta
  cualquier puerto y cualquier ruta de la máquina indicada. No es una escalada —quien llega
  al socket ya manda— pero conviene saberlo si algún día el socket se comparte.

## Ante un incidente

Aislar sin destruir pruebas:

```sh
kling stop <ref>     # mata el VMM, conserva overlay y snapshot
kling ps -a          # qué había, con su IP y su política de salida
kling topo           # de qué snapshot salió cada instancia
```

Si se sospecha del snapshot dorado, revisar las instancias antes de borrarlo: `kling rmi` se
niega si quedan vivas.
