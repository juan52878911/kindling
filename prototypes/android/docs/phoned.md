# kling-phoned: el agente del teléfono dentro de la microVM

`kling-phoned` (`prototypes/android/phoned/`, issues #89 y #92) es un binario Go
estático que sustituye a `image/android-launch.sh` y a `image/android-sh`:
arranca Android, lo relanza si muere, sirve la **API del teléfono** en un puerto
del invitado y aplica la **identidad de cada clon** tras restaurar. Con él, operar
un teléfono no exige `allow_exec` en ningún momento: ni el dorado ni sus clones
lo llevan.

```
kling-guest (PID 1 de la VM, :8080)
  └─ /entrypoint ── SERVICE: /usr/local/bin/kling-phoned  (relanzado si muere)
       ├─ :8091  API del teléfono (HTTP)          ← POST /machines/{ref}/guest
       ├─ :5555  reenvío a 127.0.0.1:5555 DENTRO del espacio de red de Android (adb)
       └─ hijo: stage2 (PID 1 de un espacio de PIDs nuevo) ── pivot_root ── exec /init
/etc/kindling/ready                  → kling-phoned ready     (sys.boot_completed=1 y pantalla preparada)
/etc/kindling/post-restore.d/10-identity → kling-phoned identity (identidad del clon, de MMDS)
```

## Qué hace el lanzador

Lo mismo que el de bash, en el mismo orden y con los mismos ficheros
(`android.conf`, `entrypoint.args`, `/run/kindling-android/{state,unshare.pid}`,
así que `android-sh`, `uidump` y `fase0.sh` siguen funcionando):

| Paso | Antes (bash) | Ahora |
|---|---|---|
| comprobar el kernel | `grep` de `/proc/filesystems` + comprobador | igual, en Go; el comprobador (`check-android-config.sh`) sigue siendo informativo |
| espacios de nombres | `unshare --mount --pid --fork --kill-child --ipc --uts [--net]` | `clone` con `CLONE_NEWNS/NEWPID/NEWIPC/NEWUTS[/NEWNET]` y `PDEATHSIG` (el hilo que hace el clone queda bloqueado mientras Android vive, para que Go no lo termine) |
| rootfs | `mount --bind`, `/proc`, `/sys`, cgroup2, `/dev` en tmpfs con `cp -ax`, devpts, shm, mqueue, binderfs | igual con `mount(2)`; `/dev` se copia recorriéndolo en Go (nodos, enlaces, FIFO, dueño y modo) |
| `/data` en RAM | `cp --sparse` + `mount -o loop` | copia con `SEEK_DATA`/`SEEK_HOLE` y `LOOP_CTL_GET_FREE` + `LOOP_SET_FD` + `LOOP_SET_STATUS64` (autoborrado) |
| `pivot_root` | con el cargador de glibc de la raíz vieja para el `umount` | `pivot_root(2)` + `umount2(MNT_DETACH)`: binario estático, no necesita cargador |
| red veth | `ip netns`/`ip link`/`ip addr`/`ip route` + iptables (DNAT, MASQUERADE, filtros) | **netlink** a mano (`RTM_NEWLINK` con el extremo `eth0` creado directamente en el espacio de red del hijo por `IFLA_NET_NS_PID`, `RTM_NEWADDR`, `RTM_NEWROUTE`), sin espacio de red con nombre; iptables solo para MASQUERADE y dos DROP (abajo) |
| adb desde fuera | DNAT de 5555 a 10.88.0.2 | reenvío en espacio de usuario: el socket de salida se abre con `setns` en el espacio de red de Android y conecta a `127.0.0.1:5555`. Android no necesita ninguna ruta hacia la VM, y con `ANDROID_NET=isolated` adb sigue funcionando |
| dm-verity | `overlay-init` de la base (`verity.sh`) | lo mismo (la capa es el rootfs: no se puede poner detrás de verity después de PID 1); kling-phoned lee el estado del objetivo con `DM_TABLE_STATUS` y `/v1/health` no da el teléfono por sano si la imagen declara verity y el estado no es `V` |
| relanzar | el bucle del entrypoint | kling-phoned relanza Android con espera creciente (1 → 16 s); si muere kling-phoned, `PDEATHSIG` mata a Android y el bucle del entrypoint relanza los dos |
| pantalla | `phone.sh` por `kling exec` | con `ANDROID_PREP=1` (por defecto con `PHONED=1`), kling-phoned la deja encendida, sin bloqueo ni animaciones tras el primer `boot_completed`, y la sonda de listo lo espera |

**Por qué queda iptables.** El kernel del prototipo no trae `nf_tables`
(`config-android` habilita xtables, que es lo que usa netd) y está fijado por
sha256, y xtables no se programa por netlink: `setsockopt(IPT_SO_SET_REPLACE)`
sustituye la tabla entera con un blob binario que depende de la versión de cada
*match* y *target*. Reimplementarlo es mantener un iptables pequeño para tres
reglas que se ponen una vez por arranque: `MASQUERADE` de lo que sale de Android,
`DROP` de todo lo que entra a la VM desde `kandroid0` (Android no llega a
kling-guest, a esta API ni a nada de la VM) y `DROP` de Android a
`169.254.0.0/16` (MMDS). Sin iptables en la base, el modo `veth` cae a
`isolated` (sin salida) en vez de dejar a Android alcanzar la VM.

**Por qué nsenter.** Las herramientas de Android (`screencap`, `pm`, `settings`,
`uiautomator`, `logcat`) corren dentro de sus espacios de nombres y con el
entorno de zygote. Un proceso Go tiene varios hilos y no puede entrar en otro
espacio de montajes (`setns(CLONE_NEWNS)` exige un solo hilo), así que para eso
se usa `nsenter` de util-linux (en la base), con los argumentos variables como
`$1`, `$2` y lo sensible por stdin. Lo que no necesita un proceso no lo usa: las
propiedades se leen de la memoria de Android (abajo), el socket de `uidump` se
abre por `/proc/<init>/root` y adb va por `setns` de red en un hilo.

## La API

HTTP/1.1 en `:8091` del invitado (`PHONED_LISTEN`). La máquina tiene que declarar
el puerto en `kling.ports` (phone.sh pone `5555,8091`): el proxy del daemon solo
llega al 8080 y a esos. Todo cuerpo binario admite base64 con
`?encoding=base64` (el proxy lleva el cuerpo como una cadena JSON, que no es
binario limpio); sin él, binario tal cual (una arista de grafo o un reenvío).

| Ruta | Qué | Cómo |
|---|---|---|
| `GET /v1/health` | `200`/`503` con `ok`, `state`, `boot_completed`, `system_server`, `android_pid`, `restarts`, `net`, `verity`, `uidump`, `adb_secure`, `kernel`, `api_tokens` (cuántos abren la API; 0 = cerrada) | sin procesos: `/proc` y la memoria de propiedades. **La única ruta sin token** |
| `GET /v1/screen` | PNG | `screencap -p` |
| `GET /v1/tree[?compressed=1]` | XML de la jerarquía; `X-Phoned-Source: uidump` o `uiautomator` | el servidor residente `uidump` si la imagen lo trae (lo arranca por init si no contesta), si no `uiautomator dump` |
| `POST /v1/tap` `{"x","y"}` · `/v1/swipe` `{"x1","y1","x2","y2","ms"}` · `/v1/text` `{"text"}` · `/v1/key` `{"key": "BACK"\|4}` | `{"ok":true,"via":"uidump"\|"input"}` | uidump; si no, `input` |
| `POST /v1/install` | el APK como cuerpo (hasta 128 MiB) | `cmd package install -r -t -S <tamaño>` por stdin: ningún fichero temporal |
| `POST /v1/launch` `{"package"}` | abre la actividad de LAUNCHER | `cmd package resolve-activity` + `am start -W` |
| `GET /v1/logs?buffer=main\|system\|crash\|events\|all\|phoned&lines=N` | texto | `logcat -d`; `phoned`: los últimos 256 KiB del propio agente |
| `GET /v1/identity` | `serial`, `android_id`, `device_name`, `adb_secure`, `adb_keys` (cuántas), `ssaid_userkey_sha256` (huella), `ssaid` (paquete → SSAID) | para comprobar clones; la clave de SSAID nunca sale, solo su huella |
| `POST /v1/verify-cache` | `200`/`409` con `ok`, `files`, `bytes`, `mismatches`, `seconds` | `android-sh --verify-cache` en Go: cada fichero de `system/{lib64,bin,framework,apex}` y `vendor/{lib64,bin}` leído por la caché de páginas y con `O_DIRECT` (del disco) tiene que dar el mismo sha256. `kling phone golden build` lo exige antes de guardar un dorado |

Todo salvo `GET /v1/health` (y el índice `GET /`) exige `Authorization: Bearer
<token>` (abajo, "Autenticación").

Ejemplo por el daemon (`kling phone api` y `phone.sh api` lo envuelven, con el
token del teléfono):

```sh
kling phone api 1 GET /v1/health
kling phone api 1 GET /v1/screen > pantalla.png          # decodifica el base64 del proxy
kling phone api 1 POST /v1/install termux.apk            # lo pasa a base64 para el proxy
phone.sh api 1 GET /v1/health
phone.sh api 1 GET '/v1/screen?encoding=base64' > pantalla.png
printf '{"x":360,"y":640}' > tap.json && phone.sh api 1 POST /v1/tap tap.json
base64 < termux.apk | tr -d '\n' > apk.b64 && phone.sh api 1 POST '/v1/install?encoding=base64' apk.b64
```

**Sin shell.** No hay ruta para ejecutar órdenes: cada operación valida sus
argumentos (coordenadas, nombres de tecla y de paquete por expresión regular,
texto de una línea, APK con la firma ZIP) y el guion de cada una es fijo. Una
shell en la API sería root en Android con solo un token de portador, es decir,
lo mismo que `allow_exec` sin su control en el daemon; quien la necesite usa
`kling exec` + `android-sh`, que sí lo exige.

## Modelo de amenazas: quién puede hablar con el 8091

La API da el control del teléfono (instalar un APK es ejecutar código en él).
Llegar al puerto no basta: hace falta un token (abajo). Quién llega al puerto:

| Quién | ¿Llega al 8091? | ¿Con token? |
|---|---|---|
| El daemon (`POST /machines/{ref}/guest`) | sí, si la máquina declara el puerto en `kling.ports` | lo pone quien llama al daemon: `kling phone` lo lee del store del daemon (ns `phone`), que solo se lee con acceso al socket, la misma confianza que el propio proxy |
| Un reenvío del Mac (vz) | sí, en `127.0.0.1` del Mac, si está en `kling.ports` | no lo tiene salvo que el usuario se lo dé: cualquier proceso local ve solo `/v1/health` |
| El anfitrión Linux | cualquier proceso del anfitrión alcanza la IP del tap | igual: solo `/v1/health` |
| Otras microVMs | no; una **arista de grafo** (`link`) al 8091 sí llega (SECURITY.md §15) | no: una arista sin token recibe `401` en todo salvo `/v1/health`. Dar el control a un nodo es darle el token (`kling phone token <tel>`, o uno de solo lectura con `-read`) |
| Android (sus apps) | no | con `veth`, `DROP` de todo lo que entra por `kandroid0`; con `isolated`, no hay ruta; el 8091 no se reenvía a Android |
| El gateway de kindling | no | reenvía MCP al 8080 y corta las rutas de control; no conoce el 8091. `kling phone mcp` sí: es un servidor MCP en el anfitrión que llama al 8091 por el proxy del daemon con el token de cada clon |

## Autenticación (#110)

**Por qué un token y no el origen.** El proxy del daemon y el proxy de enlace de
una arista marcan los dos desde el anfitrión (Linux: la misma dirección del lado
host del veth; macOS: el mismo reenvío de `kling-vz`), así que el invitado no
puede distinguir una llamada del daemon de una de un nodo del grafo. Se descartó
también un puerto de solo lectura aparte: una arista seguiría pudiendo apuntar al
de control, y un segundo servidor duplica la superficie sin cerrar nada.

**Cómo.** Toda ruta salvo `GET /v1/health` y `GET /` exige
`Authorization: Bearer <token>`. `kling-phoned` no guarda tokens, solo sus
sha256 con un ámbito, en la RAM de la VM (`/run/kindling-android/api-tokens.json`,
0600, releído en cada cambio):

| Ámbito | Rutas |
|---|---|
| `read` | `GET /v1/screen`, `GET /v1/tree` |
| `control` | todas (tocar, escribir, instalar, `launch`, `logs`, `identity`, `verify-cache`) |

Sin token válido, `401` (con `WWW-Authenticate: Bearer`); con uno de lectura en
una ruta de control, `403`. La comparación recorre todos los hashes en tiempo
constante. Un fichero ilegible cuenta como vacío: **cerrado, nunca abierto**.

**De dónde salen.** Con la identidad del clon, por MMDS (`api_tokens`, abajo): el
token no pasa por MMDS, solo su sha256. Un documento con `api_tokens` y sin
`android_id` solo cambia los tokens (rotar, acuñar uno de lectura o revocar con
`[]`) sin rehacer la identidad; la huella de "identidad ya aplicada" no los cuenta.
Viajan con la memoria en `pause` y `freeze`. **Sin tokens la API está cerrada**:
un dorado no tiene ninguno (`kling phone golden build` usa uno de un solo uso para
comprobar la caché y lo revoca antes de guardar), y un clon tampoco hasta su
gancho. Así un nodo de grafo hecho del dorado, sin identidad, no se controla por
ninguna arista.

**Quién los guarda.** `kling phone` genera el token de control de cada clon
(`kph_` + 32 bytes al azar) y lo guarda en el store del daemon (`/store/phone/<id
de la máquina>`), que solo se lee con acceso al socket: `kling phone api/view/mcp`
lo añaden solos, desde cualquier máquina que hable con el daemon. Con la política
de autorización del daemon ([`docs/authz.md`](../../../docs/authz.md)) `/store` es de
admin; un inquilino guarda entonces sus tokens en un fichero 0600 suyo
(`$XDG_CONFIG_HOME/kling/phone-tokens/<daemon>/<id>.json`). `phone.sh` los guarda
en `$PHONE_ROOT/tokens/<tel>` (0600).

**El proxy del daemon ya no llega sin token** a las rutas de control: es la
decisión de #110. Mantenerlo abierto exigiría que el invitado reconociera las
llamadas del daemon, y no puede (ver arriba). No cambia la confianza: el token
está en el store del daemon, detrás del mismo socket que el proxy. `curl` directo
al proxy sin la cabecera solo ve `/v1/health`.

```sh
kling phone token 1                  # el token de control (para dárselo a quien deba controlar el teléfono)
kling phone token 1 -read            # acuña uno de solo lectura (screen, tree) y lo imprime
kling phone token 1 -rotate          # nuevo token de control; los anteriores dejan de valer
kling phone api 1 -no-token GET /v1/tree    # lo que ve una arista sin token: 401
```

Una arista `link` hacia el 8091 de un teléfono (el nodo `ctl` le pasa el token en
la cabecera; se lo da el operador, p. ej. `kling phone token tel | kling machine
secret <ctl>`, que lo deja en el MMDS de `ctl`):

```yaml
nodes:
  ctl: {image: toolchain, allow_exec: true}
  tel: {from: phone-golden, ports: [5555, 8091]}
edges:
  - {from: ctl, to: tel, kind: link, port: 8091}
```

## Identidad por clon (#92)

La entrega el núcleo tras restaurar: `kling machine secret <m> -hooks` pone el
documento en MMDS y lanza los ganchos; `printf '{}' | kling machine secret <m>`
lo vacía después y, como el gancho salió con 0, el daemon levanta `has_secrets`.

```json
{"phone": {"android_id": "<16 hex>", "name": "phone-3", "serial": "<6-20 alfanuméricos>",
           "adb_keys": ["<línea de adbkey.pub>"], "ssaid": "regen", "ssaid_key": "<64 hex>",
           "api_tokens": [{"sha256": "<64 hex>", "scope": "control"}]}}
```

Solo `android_id` es obligatorio; sin `serial` ni `ssaid_key` se sacan del CRNG
del kernel ya resembrado; sin `api_tokens`, los tokens que hubiera no se tocan (y
sin ninguno la API sigue cerrada). Qué hace `kling-phoned identity`, en orden:

0. **Tokens de la API** (auth.go): los escribe primero, aparte. Un documento
   solo con `api_tokens` termina aquí.

1. **Serie.** `ro.serialno` es de solo lectura para `setprop`: init la fija al
   arrancar desde `androidboot.serialno` (kling-phoned arranca el dorado con
   `KINDLINGGOLDEN`, así la propiedad existe). Se reescribe **en su sitio en la
   memoria de propiedades** (`/dev/__properties__`, formato de bionic: copia al
   área de respaldo, serial sucio, valor, serial nuevo y `FUTEX_WAKE`), como
   `resetprop` de Magisk, junto con `ro.boot.serialno`. Cada proceso la ve al
   instante; va antes del reinicio del framework para que system_server
   arranque ya con ella.
2. **SSAID por app.** El `ANDROID_ID` de cada app (Android 8+) es
   `HMAC-SHA256(userkey, firma de la app)`, con `userkey` en
   `/data/system/users/0/settings_ssaid.xml`. SettingsProvider la crea la
   primera vez que una app pide su `ANDROID_ID`, con el `SecureRandom` de
   system_server, cuyo estado (el DRBG de BoringSSL en su memoria) es el mismo
   en todos los clones de un dorado: resembrar el kernel no lo toca. Así que el
   gancho escribe una clave suya (por stdin, en ABX con `xml2abx`), parando
   zygote antes (`ctl.stop zygote` se lleva a system_server) y arrancándolo
   después; espera a un `sys.boot_completed=1` nuevo. `"ssaid": "keep"` se lo
   salta. Es lo que cuesta: **~4,6 s** en Firecracker.
3. **`android_id` legado y nombre** (`settings put`), como el gancho de bash.
4. **adb con claves.** La imagen arranca con `ro.adb.secure=1`
   (`ADB_SECURE=1`, por defecto con `PHONED=1`); el gancho escribe
   `/data/misc/adb/adb_keys` con las claves públicas de ESTE clon (por stdin;
   `system:shell 0640`). Sin claves en el documento, el fichero se borra y nadie
   entra por adb. adbd lee el fichero en cada intento, sin reiniciar. El dorado
   no tiene ninguna.

El documento se aplica una vez: su huella (sha256 del documento recibido) queda
en `/run/kindling-android/identity.sha256`, en la RAM de la VM, y un `thaw` con el
mismo documento aún en MMDS no regenera nada. La salida del gancho (consola,
`kling logs`) dice qué se aplicó, nunca los valores, y cualquier error pasa por
un filtro que los tacha.

**Lo que sigue igual entre clones**: las claves de firma de apps instaladas en el
dorado, los tokens que haya en `/data` del dorado (ninguno en uno recién
arrancado), la MAC de `eth0` de Android (la del veth, que se crea al arrancar y
viaja en el dorado) y el `Build.SERIAL` en caché de procesos ya arrancados
antes del gancho (en Android 8+ las apps no lo ven: `Build.getSerial()` pregunta
a system_server, que se reinicia).

## Construir y comparar

```sh
sudo prototypes/android/image/build-image.sh                 # PHONED=1: kling-phoned (por defecto)
sudo PHONED=0 prototypes/android/image/build-image.sh        # el lanzador de bash, para comparar
```

`build-image.sh` busca `kling-phoned` en la raíz del repo o en `KLING_PHONED`, y
si hay Go lo compila (`CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -o
kling-phoned ./prototypes/android/phoned`). La receta lleva `spec.phoned` y
`spec.adb_secure`.

## Cifras (2026-09-29)

`test-phoned.sh` (sin `allow_exec` en ningún momento), con daemons privados y los
binarios de la rama. Mac: MacBook Air M4, vz, arm64, 2 vCPU, 1,5 GiB. Linux: el CT
`phones` (x86_64, Firecracker, 6 GiB), con dm-verity.

| | Mac arm64 (vz) | Linux amd64 (Firecracker) |
|---|---|---|
| dorado: `boot_completed` en frío (+ pantalla preparada) | 7,7 s | 15,0 s |
| `kling save` del dorado | 1,3 s | 3,6 s |
| restaurar un clon (`run -from -wait-ready`) | 1,1–1,5 s | 0,2–2,2 s |
| identidad (serie + SSAID con reinicio de zygote + android_id + adb) | 2,2–2,7 s | 5,1–7,0 s (SSAID 4,6 s) |
| `GET /v1/health` | 0,03 s | 0,04 s |
| `GET /v1/screen` (PNG 720×1280) | 0,55–0,68 s | 0,24–0,56 s |
| `GET /v1/tree` (uidump) | 0,07–0,09 s | 0,02–0,17 s |
| `tap` / `key` / `swipe` / `text` (uidump) | 0,04 / 0,04 / 0,24 / 0,95 s | 0,04 / 0,04 / 0,24 / 0,27 s |
| `install` de Termux (35 MB, base64 por el proxy) | 0,87 s | 2,0–2,2 s |
| pause → thaw → `screen` por la API | 0,01 s → 0,16–0,26 s | 0,01 s → 0,25–0,27 s |
| freeze → thaw → listo | 1,1–1,3 s | 0,3–0,5 s |
| red | veth (NAT; egress `none`) | veth, `verity: verified` |

En los dos: 3 clones con serie, `android_id` y clave de SSAID distintas; adb con la
clave del clon entra (`ro.serialno` = la de `/v1/identity`), con la de otro clon o sin
clave, `unauthorized`; `has_secrets` levantada tras el `{}`; la identidad intacta tras
pause y freeze; ni series ni `android_id` en `kling ps -json`, `kling logs`, los logs
del daemon ni los ficheros del dorado; el dorado sin `allow_exec` (y `kling run -from`
con `-allow-exec` sobre él, rechazado por el núcleo).

Red (Linux, `-egress internet`, comprobado con `kling exec` en una máquina de
depuración): Android sale (`HTTP/1.1 301` de 1.1.1.1, ping a 8.8.8.8, la red de
Android `IS_VALIDATED`) y no llega a `10.88.0.1:8080/8091`, `172.16.0.2:8080/8091`
ni `169.254.169.254:80` (timeout: `DROP`).

Sin romper lo de antes: `test-phone.sh` PASS y `fase0.sh -clones 2` 6/6 en el Mac con
la imagen nueva (frío 6,2 s, restaurar → dump p50 1,06 s, dump 0,024 s, screencap
0,31 s, fork 3,9 s); `PHONED=0` sigue construyendo y arrancando (listo en 10,2 s en
Linux, `android-sh` y el gancho de bash funcionan).

Pendiente:
- SSAID: comprobado que system_server arranca con la clave nueva (sin errores de
  SettingsProvider) y que cada clon tiene la suya; no se ha visto a una app pedir su
  `ANDROID_ID` (ninguna de la imagen lo hace y `run-as`/`su <uid> content` no sirven
  en Redroid). Hace falta un APK de prueba mínimo.
- Una arista de grafo al 8091 funcionaría hoy, sin autenticación (ver el modelo de
  amenazas): token por arista o puerto de solo lectura antes de usarla.
- La base arm64 construida antes del 28-09 no trae iptables: kling-phoned cae a
  `isolated` (Android sin salida, adb sigue); una base nueva de `build-image.sh` sí.

