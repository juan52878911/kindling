# Proxy de credenciales en un proceso aparte (macOS)

Estado: **diseño, sin implementar** (issue #80). Este documento es el plan que cita
[`SECURITY.md`](../SECURITY.md) (sección 7 y "Lo que NO está resuelto").

## El problema

En Linux la clave real de una credencial no sale nunca del daemon: el proxy
(`pkg/credproxy`) es una goroutine suya, escucha en el lado host del veth, y el VMM
(Firecracker, sin privilegios y en su jail) no la ve.

En macOS no hay veth ni netns: la red del invitado es la pila gVisor que corre dentro
de `kling-vz`, y el proxy se sirve ahí mismo (`vz/internal/vnet`: un listener en
pasarela:80 y `ServeDB` para los puertos de Postgres/MySQL). El daemon le entrega las
claves por `PUT /kling/credentials` y viven en la memoria de ese proceso. Es el mismo
proceso que:

- termina TCP/UDP del invitado (gVisor), contesta su DNS y sirve MMDS;
- habla con Virtualization.framework (virtio, consola, vsock, carpetas compartidas);
- tiene salida a internet si la máquina la tiene (`NET=1` en `kling-vz.sb`).

Un invitado hostil que encuentre un fallo explotable en cualquiera de esas piezas
ejecuta código en `kling-vz` y lee la clave de su memoria. El perfil de sandbox limita
ficheros y red, pero no protege la memoria del propio proceso.

## Qué se propone

Un proceso nuevo por máquina con credenciales, **`kling-credproxy`**, que es el único
que tiene las claves. `kling-vz` se queda sin ellas: solo sabe qué dominios desviar y
por dónde pasar cada conexión.

```
invitado ──TCP──▶ kling-vz (gVisor) ──socket Unix, por conexión──▶ kling-credproxy ──▶ proveedor
                    │ DNS: dominio → pasarela                         ▲ claves, política,
                    │ sin claves                                      │ registro de auditoría
                    └──────────── daemon ─── PUT credenciales ────────┘
```

### Modelo de procesos

- **Uno por máquina**, no uno por daemon: una fuga en uno expone las claves de una sola
  máquina, igual que hoy. Solo existe si la máquina tiene credenciales (allowlist o
  aristas `credential`); sin ellas no se arranca.
- **Lo lanza el daemon**, no `kling-vz`: el daemon ya lleva la vida de `kling-vz`
  (arranque, readopción tras reiniciarse, `kill` al parar) y el mismo código sirve para
  el nuevo proceso. Se lanza al entregar las credenciales y muere con la máquina
  (stop, freeze, rm). Tras un thaw se lanza otro: no guarda estado del invitado, así
  que no hay nada que restaurar, solo volver a entregarle las claves (lo que ya hace
  `reentregarCredenciales`).
- **Readopción**: como `kling-vz`, sobrevive a un reinicio del daemon; el daemon lo
  encuentra por su socket en el directorio de la máquina y comprueba con
  `LOCAL_PEERCRED` + ruta del ejecutable que es el suyo (lo que ya hace `peercred` con
  `kling-vz`).
- **Binario firmado con hardened runtime y sin `get-task-allow`**: otro proceso del
  mismo usuario, `kling-vz` incluido, no puede pedir su `task_for_pid` para leer su
  memoria. Es la pieza que hace que la separación valga algo: sin ella, un `kling-vz`
  comprometido (mismo uid) leería la memoria del otro.

### IPC

Dos sockets Unix en el directorio de la máquina, 0600:

1. **`credproxy-api.sock`, daemon → kling-credproxy.** Las credenciales completas (lo
   que hoy es `PUT /kling/credentials` de `kling-vz`), el modo de salida de la máquina
   y el socket del broker. Solo el daemon: el proceso comprueba el uid y el ejecutable
   del otro extremo con `LOCAL_PEERCRED` y acepta solo al daemon, nunca a `kling-vz`.
   Así un `kling-vz` comprometido no puede ni cambiar las credenciales ni leerlas, ni
   decirle al proxy que la máquina está en allowlist cuando está en `none`.
2. **`credproxy.sock`, kling-vz → kling-credproxy, una conexión por cada conexión del
   invitado.** Las conexiones del invitado son de gVisor, no del kernel: no hay
   descriptor que pasar con `SCM_RIGHTS`, así que `kling-vz` abre una conexión Unix
   por cada TCP aceptado en la pasarela y copia bytes en los dos sentidos. Delante, una
   cabecera fija y pequeña (versión, puerto de destino en la pasarela: 80, 5432, 3306…,
   y nada más; longitud acotada, se rechaza lo que no cuadre). El proxy no se fía de
   la cabecera más de lo que hoy se fía del puerto: con un puerto que no es de ninguna
   credencial, cierra.

`kling-vz` recibe del daemon solo `{dominio, marcador}` (para el DNS y MMDS) y deja de
anunciar `credential_kinds` propios: los anuncia `kling-credproxy`. Una capacidad nueva
en `/kling/info` de `kling-vz` (`credproxy-split`) dice que sabe reenviar; sin ella el
daemon sigue con el modelo actual (compatibilidad con un `kling-vz` sin recompilar).

### Perfil de sandbox (`kling-credproxy.sb`)

Se aplica a sí mismo al arrancar, como `kling-vz` (`sandbox_init_with_parameters`, que
necesita cgo: el binario vive en el módulo `vz/`, no en el núcleo, que sigue sin cgo).

```scheme
(version 1)
(deny default)
(import "system.sb")
(allow process-info* (target self))
(allow sysctl-read)
;; Verificar certificados: Security.framework habla con trustd. Solo eso.
(allow mach-lookup (global-name "com.apple.trustd") (global-name "com.apple.trustd.agent"))
(allow file-read*
  (literal "/etc/resolv.conf") (literal "/private/etc/resolv.conf")
  (subpath "/System/Library/Keychains") (subpath "/Library/Keychains"))
;; Su registro de auditoría y sus dos sockets, nada más del disco.
(allow file-read* file-write* (literal (param "AUDIT")) (literal (string-append (param "AUDIT") ".1")))
(allow network-bind network-inbound (local unix-socket (path-literal (param "API_SOCK"))))
(allow network-bind network-inbound (local unix-socket (path-literal (param "FWD_SOCK"))))
(allow network-outbound (remote unix-socket (path-literal (param "BROKER"))))
;; Salida: al proveedor y al DNS del upstream (resuelve él, como hoy kling-vz).
(allow network-outbound (remote ip "*:*"))
```

Frente a `kling-vz.sb`: sin `iokit-open` ni Virtualization.framework, sin lectura de
la raíz de kindling (kernel, imágenes, dorados, snapshots, volúmenes), sin
`mach-lookup` general (hoy `kling-vz` lo tiene abierto), sin loopback de entrada.
La salida sigue siendo `*:*`: el sandbox no filtra por dominio, eso lo hace el propio
proxy (su `Lookup` con `egress.PublicIPv4` y `upstream.go`), igual que hoy.

Con el registro de auditoría en el perfil del proxy, `kling-vz.sb` puede además
**negar** la escritura en esa ruta (una regla `deny file-write*` después de la de
`MDIR`): el registro deja de estar al alcance del proceso que atiende al invitado, que
es lo que #79 hizo en Linux.

### Qué ataque evita y cuál no

Evita:

- **RCE en `kling-vz` → clave.** Un fallo en gVisor, el DNS, MMDS, virtio, vsock o las
  carpetas compartidas da un proceso sin claves, como antes de que el proxy viviera en
  macOS. Lo más que consigue es lo que ya puede el invitado: hablar con el proxy por
  el socket de reenvío, que sustituye marcadores solo hacia los dominios y rutas
  permitidos.
- **Salida a través del proxy en una máquina `none`.** El modo de salida lo dice el
  daemon, no `kling-vz`.
- **Manipular el registro de auditoría** desde `kling-vz` (con la regla de negación).

No evita:

- **Un fallo en el propio proxy** (parser HTTP de `net/http`, el protocolo de Postgres
  o MySQL de `pkg/credproxy`): ese código sigue recibiendo bytes del invitado y es
  donde está la clave. La superficie es mucho menor que la de `kling-vz`, y es la misma
  que en Linux (donde además corre como root en el daemon).
- **Otro proceso del usuario con privilegios de depuración** o root: igual que hoy.
- **Uso legítimo abusivo**: el invitado sigue pudiendo usar la credencial dentro de su
  allowlist; para eso están `Allow` y el registro.

### Coste

- **Memoria**: un proceso Go más por máquina *con credenciales*: ~8–15 MiB de RSS
  (runtime, TLS, buffers). Las máquinas sin credenciales no pagan nada. En el M4, con
  una máquina restaurada en ~350 MiB (`docs/vz-mac-prototipo.md`), es un +3–4 %.
- **Latencia**: un salto Unix y dos copias más por conexión. Del orden de decenas de
  µs por conexión y de un memcpy por byte; frente a los milisegundos del proveedor, no
  se nota. Las conexiones de Postgres/MySQL son largas: el coste es por byte, no por
  consulta.
- **Arranque**: lanzar y confinar el proceso, ~10–20 ms, solo al entregar credenciales
  (arranque, thaw); en paralelo con `snapshot/load`, fuera del camino crítico del
  thaw si se lanza antes de cargar el estado.
- **Complejidad**: un binario más que firmar y distribuir, su perfil, su readopción y
  una capacidad de compatibilidad. Es lo que más pesa.

## Por qué no está en este lote

Hacerlo bien exige verificar en un Mac real, con `kling-vz` firmado y confinado, cosas
que un test con falsos no prueba: que `x509` verifica certificados dentro del perfil
(las búsquedas a `trustd`), que el hardened runtime impide de verdad el
`task_for_pid` desde `kling-vz`, la readopción tras reiniciar el daemon y el coste
medido con `scripts/92-e2e-mac.sh`. Partirlo a medias (el proceso sin el perfil, o el
perfil sin el hardened runtime) no protegería nada y añadiría un proceso. Se deja
diseñado.

## Pasos para implementarlo

1. `vz/cmd/kling-credproxy`: `credproxy.New` + su API (`PUT /credentials`, `GET /info`)
   en `credproxy-api.sock`, el servidor de reenvío en `credproxy.sock`, `Lookup` con
   el resolver de `vz/internal/egress`, `DialMachine` por el broker. Tests con falsos.
2. `kling-credproxy.sb` + `confinar` (compartido con `kling-vz`), y la negación de
   escritura del registro en `kling-vz.sb`.
3. `vz/internal/vnet`: con `credproxy-split`, el listener de la pasarela y `ServeDB`
   reenvían al socket en vez de llamar al proxy.
4. Daemon (`internal/machine/plataforma_vz.go`): lanzar, readoptar y matar el proceso;
   entregarle las claves a él y a `kling-vz` solo dominios y marcadores; caer al modelo
   actual si `kling-vz` no anuncia `credproxy-split`.
5. Firma con hardened runtime (`scripts/` de release), y `92-e2e-mac.sh`: la sección 6c
   entera con el proxy separado, más una prueba de que `kling-vz` no puede leer la
   memoria del proxy ni escribir el registro.
