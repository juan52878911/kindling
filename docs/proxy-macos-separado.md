# Proxy de credenciales en un proceso aparte (macOS)

Estado: **implementado** (issue #80). Este documento es el diseño que cita
[`SECURITY.md`](../SECURITY.md) (sección 7 y "Lo que NO está resuelto"), con lo que
cambió al hacerlo y por qué, y lo medido en un Mac M4.

## El problema

En Linux la clave real de una credencial no sale nunca del daemon: el proxy
(`pkg/credproxy`) es una goroutine suya, escucha en el lado host del veth, y el VMM
(Firecracker, sin privilegios y en su jail) no la ve.

En macOS no hay veth ni netns: la red del invitado es la pila gVisor que corre dentro
de `kling-vz` (o `kling-hv`), y hasta ahora el proxy se servía ahí mismo
(`vz/internal/vnet`: un listener en pasarela:80 y `ServeDB` para los puertos de
Postgres/MySQL). El daemon le entregaba las claves por `PUT /kling/credentials` y
vivían en la memoria de ese proceso. Es el mismo proceso que:

- termina TCP/UDP del invitado (gVisor), contesta su DNS y sirve MMDS;
- habla con Virtualization.framework (virtio, consola, vsock, carpetas compartidas);
- tiene salida a internet si la máquina la tiene (`NET=1` en `kling-vz.sb`).

Un invitado hostil que encuentre un fallo explotable en cualquiera de esas piezas
ejecuta código en `kling-vz` y lee la clave de su memoria. El perfil de sandbox limita
ficheros y red, pero no protege la memoria del propio proceso.

## Qué se hizo

Un proceso nuevo por máquina con credenciales, **`kling-credproxy`**
(`vz/cmd/kling-credproxy`), que es el único que tiene las claves. `kling-vz` se queda
sin ellas: solo sabe qué dominios desviar y por dónde pasar cada conexión.

```
invitado ──TCP──▶ kling-vz (gVisor) ──socket Unix, por conexión──▶ kling-credproxy ──▶ proveedor
                    │ DNS: dominio → pasarela                         ▲ claves, modo de salida,
                    │ sin claves                                      │ registro de auditoría
                    └──────────── daemon ─── PUT credenciales ────────┘
```

### Modelo de procesos

- **Uno por máquina**, no uno por daemon: una fuga en uno expone las claves de una sola
  máquina, igual que antes. Solo existe si la máquina tiene credenciales (allowlist o
  aristas `credential`); sin ellas no se arranca.
- **Lo lanza el daemon** (`internal/machine/credproxy_vz.go`), no `kling-vz`, que está
  confinado y no puede lanzar procesos: al entregar las credenciales
  (`registrarCredencialesPlataforma`), desacoplado (setsid), con su salida en
  `machines/<id>/credproxy.log` y el entorno del VMM (sin `KLING_TOKEN` ni nada de la
  sesión). El binario va junto al VMM (`bin/kling-credproxy`).
- **Muere con su VMM, solo** (cambio sobre el diseño): el proxy vigila el PID de su VMM
  con kqueue (`EVFILT_PROC`, `NOTE_EXIT`, registrado antes de confinarse) y sale en
  cuanto muere. Stop, freeze y rm matan al VMM y se llevan al proxy también con el
  daemon caído, que es justo cuando un `kill` del daemon no llegaría. El daemon solo
  mata a uno que siga vivo con otro VMM (no debería pasar). Tras un thaw o un start el
  VMM es otro y el daemon lanza otro proxy al devolverle las claves
  (`redAntesDeArrancar`, `reentregarCredenciales`), antes de que el invitado corra.
- **Readopción**: sobrevive a un reinicio del daemon. Al reconciliar, el daemon
  comprueba que el socket del proxy lo atiende un `kling-credproxy` y lo dice
  (`credential proxy readopted`); la siguiente entrega de credenciales usa ese mismo
  proceso. Si no le contesta (solo admite el ejecutable del daemon que lo lanzó, y una
  actualización por Homebrew lo cambia de ruta), el daemon lo cambia por uno nuevo y
  le vuelve a dar las claves: no guarda nada que no se le pueda volver a dar.
- **Binario firmado con hardened runtime y sin `get-task-allow`**
  (`vz/build-credproxy.sh`, que falla si la firma no lo dice). Medido (ver abajo): lo
  que impide el `task_for_pid` desde otro proceso del mismo usuario es que no tenga
  `get-task-allow`; el hardened runtime añade que no se le inyecten bibliotecas
  (`DYLD_*`) ni cargue código sin firmar.

### IPC

Dos sockets Unix 0600 en **`machines/<id>/credproxy/`** (cambio sobre el diseño, que
los ponía en el directorio de la máquina): un subdirectorio 0700 que crea el daemon y
que es solo del proxy. Así una regla del perfil de `kling-vz` le niega escribir en
todo él (sockets, registro con todas sus rotaciones, cuerpos derramados) sin una regla
por fichero, y `kling-vz` no puede crear el directorio antes que el daemon ni plantar
un socket falso donde el daemon entrega las claves. Los nombres son cortos por
sun_path (104 bytes): el proxy se ata desde su directorio, el daemon conecta por el
enlace corto de `internal/fc`, y `kling-vz` trabaja desde el directorio de su máquina
cuando la ruta no cabe.

1. **`api.sock`, daemon → kling-credproxy.** `GET /kling/info`, `PUT /kling/network`
   (el modo de salida) y `PUT /kling/credentials` (las credenciales completas), con los
   mismos cuerpos que las de `kling-vz` para que el daemon use el mismo cliente. El
   proxy solo acepta conexiones de este usuario cuyo ejecutable es el del daemon
   (`LOCAL_PEERCRED` + `LOCAL_PEERPID` + `proc_pidpath`, el daemon se lo dice en
   `KLING_CREDPROXY_DAEMON`), nunca a `kling-vz`: un `kling-vz` comprometido no puede ni
   cambiar las credenciales, ni leerlas, ni decirle al proxy que la máquina está en
   allowlist cuando está en `none`. **Y al revés** (añadido): el daemon, en cada
   conexión, comprueba que al otro lado está el `kling-credproxy` que hay junto al VMM y
   que su línea de órdenes es la del VMM de esa máquina (`--vmm-pid`); si no, no le da
   nada y lo dice.
2. **`fwd.sock`, kling-vz → kling-credproxy, una conexión por cada conexión del
   invitado.** Las conexiones del invitado son de gVisor, no del kernel: no hay
   descriptor que pasar con `SCM_RIGHTS`, así que `kling-vz` abre una conexión Unix por
   cada TCP aceptado en la pasarela (`vz/internal/reenvio`) y copia bytes en los dos
   sentidos. Delante, una cabecera fija de 4 bytes (`'K'`, versión 1, puerto de la
   pasarela en big endian; `pkg/credproxy/separado.go`): se rechaza lo que no cuadre, y
   el proxy no se fía del puerto más de lo que se fiaba del de la pila (el 80 va al
   proxy HTTP; otro, al de bases de datos solo si hay alguna credencial de base de
   datos; si no, cierra). Solo acepta al ejecutable del VMM (`KLING_CREDPROXY_VMM`).

`kling-vz` recibe del daemon solo `{dominios, si alguna es de base de datos, si todas
van a otra máquina}` por `PUT /kling/credproxy` (cambio: tampoco los marcadores, que ya
los pone el daemon en MMDS). Anuncia en `/kling/info` la capacidad `credproxy-split`;
sin ella el daemon sigue con el modelo de antes (compatibilidad con un `kling-vz` sin
recompilar, verificada con el `kling-hv` de main). `credential_kinds` los dice el
proxy en su propio `/kling/info`, y el daemon los comprueba contra él. Si `kling-vz`
anuncia la capacidad y no hay `kling-credproxy` a su lado, el daemon **no** cae al
modelo anterior: falla y dice qué instalar (una bajada silenciosa de seguridad no).

**El broker** (`broker_vz.go`, añadido): las conexiones de las aristas `credential`
(`kling db attach`) y de un `-upstream` en el loopback del Mac las pide ahora el proxy,
no `kling-vz`. El broker lo reconoce por su ejecutable (junto al VMM) y su línea de
órdenes, y lo trata como a la máquina de su VMM, con su directorio como comprobación.

### Perfil de sandbox (`kling-credproxy.sb`)

Se aplica a sí mismo después de abrir sus sockets y antes de atender nada
(`sandbox_init_with_parameters`, que necesita cgo: el binario vive en el módulo `vz/`;
el trozo con cgo está en `vz/internal/sbinit`, para que el proxy no arrastre la pila de
red de los VMM). Parámetros `DIR` (su directorio) y `BROKER`. Frente a lo diseñado:

- `process-info-pidinfo`, para comprobar quién está al otro lado de sus sockets;
- `file-read-metadata` y la zona horaria;
- trustd ya lo da `system.sb`; las reglas de `mach-lookup` y de los almacenes de
  certificados se dejan explícitas. **Verificado**: el proxy verifica el TLS de
  httpbin.org desde dentro del perfil (basic-auth 200).
- sin `/etc/resolv.conf`: el resolver es el de `vz/internal/egress`, que pregunta a
  1.1.1.1, nunca al del Mac.

Frente a `kling-vz.sb`: sin Virtualization.framework, sin lectura de la raíz de kindling
(kernel, imágenes, dorados, snapshots, volúmenes, `secrets/`) ni del resto del
directorio de la máquina (discos, memoria del invitado), sin escuchar en TCP. La salida
sigue siendo `*:*` (menos el loopback del Mac): el sandbox no filtra por dominio, eso lo
hace el propio proxy (su `Lookup` con `egress.PublicIPv4` y `upstream.go`), igual que
antes. `sandbox_init` del perfil: ~10 ms.

`kling-vz.sb` y `kling-hv.sb` niegan escribir en `MDIR/credproxy` (ni ficheros, ni
sockets, ni el directorio), leer `MDIR/credproxy/tmp` (los cuerpos derramados llevan la
clave) y escribir el `credaudit.jsonl` que dejaban los VMM anteriores; y dejan conectar
a `MDIR/credproxy/fwd.sock`. Probado con los perfiles de verdad en
`vz/internal/sandbox/perfil_credproxy_darwin_test.go`.

### El registro de auditoría

El proxy escribe el suyo en `machines/<id>/credproxy/credaudit.jsonl`, fuera del alcance
de `kling-vz`. Pero `kling-vz` también registra lo que atiende él (aristas link, puentes
al host y las negativas de su proxy sin claves): eso va ahora a
`machines/<id>/vmmaudit.jsonl` (cambio: dos procesos rotando el mismo fichero se
pisarían). El daemon mezcla por hora el del proxy, el del VMM y el que dejaban los VMM
anteriores (`machines/<id>/credaudit.jsonl`), y de los dos que `kling-vz` puede escribir
descarta toda línea que diga haber usado una credencial posterior a la creación del
directorio del proxy: desde entonces quien usa las claves es el proxy. Un `kling-vz`
tomado puede añadir líneas de lo que atiende él; no borrar ni cambiar las del proxy.

### Qué ataque evita y cuál no

Evita:

- **RCE en `kling-vz` → clave.** Un fallo en gVisor, el DNS, MMDS, virtio, vsock o las
  carpetas compartidas da un proceso sin claves, como antes de que el proxy viviera en
  macOS. Lo más que consigue es lo que ya puede el invitado: hablar con el proxy por
  el socket de reenvío, que sustituye marcadores solo hacia los dominios y rutas
  permitidos. Tampoco consigue el `task_for_pid` del proxy (medido) ni sus cuerpos
  derramados (perfil).
- **Salida a través del proxy en una máquina `none`.** El modo de salida lo dice el
  daemon, no `kling-vz`.
- **Manipular el registro de auditoría del proxy** desde `kling-vz`.
- **Suplantar al proxy** ante el daemon con un socket propio.

No evita:

- **Un fallo en el propio proxy** (parser HTTP de `net/http`, el protocolo de Postgres
  o MySQL de `pkg/credproxy`): ese código sigue recibiendo bytes del invitado y es
  donde está la clave. La superficie es mucho menor que la de `kling-vz`, y es la misma
  que en Linux (donde además corre como root en el daemon).
- **Otro proceso del usuario con privilegios de depuración** o root: igual que antes.
- **Uso legítimo abusivo**: el invitado sigue pudiendo usar la credencial dentro de su
  allowlist; para eso están `Allow` y el registro.

## Coste medido

En un Mac M4 (macOS 26.5), kling-hv, una máquina de 256 MiB con la imagen e2e-tools,
tres pasadas de la prueba centrada (abajo). "Antes" es el `kling-hv` de main con el
mismo daemon (cae al modelo anterior).

| | antes (proxy en el VMM) | con `kling-credproxy` |
|---|---|---|
| RSS del proxy | — | 16,4–20,0 MiB recién usado, 20,2–23,6 MiB tras ~520 conexiones más |
| RSS de `kling-hv` con la máquina | 137–139 MiB | 111–132 MiB |
| invitado → proxy, una conexión por petición (403 sin proveedor), mediana / p90 | 0,12–0,14 / 0,14–0,23 ms | 0,18–0,34 / 0,27–1,03 ms |
| thaw con credenciales (`kling thaw`) | 132–147 ms | 156–235 ms |
| primera `machine credential` (lanza el proxy) | — | 153–513 ms |
| `sandbox_init` del perfil | — | ~10 ms |

- **Memoria**: un proceso Go más por máquina *con credenciales*: ~16–24 MiB de RSS, por
  encima de los 8–15 estimados (el runtime, TLS y Security.framework, parte de él
  compartido entre procesos). Las máquinas sin credenciales no pagan nada. Con una
  máquina restaurada en ~350 MiB (`docs/vz-mac-prototipo.md`), un +5–7 %.
- **Latencia**: un salto Unix, la comprobación del otro extremo y dos copias más por
  conexión: +0,05–0,19 ms de mediana por conexión nueva, frente a los milisegundos del
  proveedor. Las conexiones de Postgres/MySQL son largas: el coste es por byte, no por
  consulta.
- **Arranque**: el thaw lanza y confina el proxy antes de cargar el estado, en serie
  (no en paralelo como se propuso): +10–90 ms en el thaw de una máquina con
  credenciales, que no estaba en el camino crítico de una sin ellas.
- **Complejidad**: un binario más que firmar y distribuir (release, `install.sh`,
  Homebrew, `kling upgrade`), su perfil, su readopción, el broker y una capacidad de
  compatibilidad.

## Qué se verificó, y cómo

Con falsos (`go test -race`):

- `pkg/credproxy/separado_test.go`: la cabecera (ida y vuelta, rechazos) y el servidor
  de reenvío (HTTP con keep-alive, cierre de lo que no es suyo, `Close` con conexiones a
  medias).
- `vz/internal/reenvio`, `vz/internal/vnet` (`TestSplitOnGateway`) y
  `vz/internal/server` (`TestKlingCredproxy`): el reenvío y cuándo se enciende.
- `vz/cmd/kling-credproxy` (`TestProcesoEntero`): el proceso entero en un hijo, con su
  perfil de verdad: API solo para el «daemon» (curl no llega), 403 y registro, sale al
  morir su VMM.
- `vz/internal/sandbox/perfil_credproxy_darwin_test.go`: los perfiles de verdad, del
  lado del VMM (conecta a `fwd.sock`; no escribe, borra ni ata nada en
  `MDIR/credproxy`; no lee los derramados) y del proxy (su directorio y el broker, nada
  más).
- `internal/machine` (`credproxy_vz_test.go`, `credaudit_vz_test.go`): con un VMM y un
  `kling-credproxy` de mentira, las claves van al proxy con el modo antes, el VMM solo
  recibe dominios, una segunda entrega reusa el proceso, muere con su VMM, un socket
  ajeno no recibe nada, uno que no contesta a este daemon se cambia por otro, un VMM anterior sigue con el modelo de antes, el broker
  reconoce al proxy, y la mezcla del registro descarta lo que un VMM inventa.

En el M4, lo que un falso no prueba (una VM a la vez, bajo `/tmp/kling-vm.lock`, con un
daemon privado y `KLING_VMM=hv`):

- La sección 6c de `scripts/92-e2e-mac.sh` (proxy contra httpbin.org), con la misma
  sonda en el invitado, en una prueba centrada que añade lo de abajo: 44 de 44, tres
  veces.
- x509 verifica dentro del perfil: basic-auth 200 con la clave real sobre HTTPS.
- `task_for_pid` contra el proxy desde un proceso del mismo usuario **sin** sandbox:
  `(os/kern) failure`. Testigos: un binario ad-hoc con `get-task-allow`, éxito; sin él
  (con o sin hardened runtime), fallo.
- Readopción: el proxy sobrevive a parar el daemon, el nuevo lo readopta (mismo PID), la
  credencial sigue funcionando y una nueva va al mismo proceso.
- Muere con freeze, stop y rm (su PID desaparece); tras thaw y start hay uno nuevo y la
  credencial funciona con el mismo marcador; el registro conserva las peticiones de
  antes del freeze.
- La clave no aparece en el log del daemon, ni en el de `kling-hv`, ni en el del proxy;
  el registro está en 0600 y el directorio del proxy en 0700.
- Con el `kling-hv` de main (sin `credproxy-split`), el mismo daemon cae al modelo
  anterior y la credencial funciona.

No verificado en el Mac: kling-vz (Virtualization.framework) en lugar de kling-hv (el
código del reenvío es el mismo, `vz/internal/server` y `vnet`), y los tipos postgres,
mysql, sigv4 y las aristas credential de extremo a extremo (las secciones 6e, 6f y 6h
de `92-e2e-mac.sh` piden más de una VM o un servidor de bases de datos); sí con falsos.
