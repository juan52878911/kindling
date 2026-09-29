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
| `GET /v1/health` | `200`/`503` con `ok`, `state`, `boot_completed`, `system_server`, `android_pid`, `restarts`, `net`, `verity`, `uidump`, `adb_secure` | sin procesos: `/proc` y la memoria de propiedades |
| `GET /v1/screen` | PNG | `screencap -p` |
| `GET /v1/tree[?compressed=1]` | XML de la jerarquía; `X-Phoned-Source: uidump` o `uiautomator` | el servidor residente `uidump` si la imagen lo trae (lo arranca por init si no contesta), si no `uiautomator dump` |
| `POST /v1/tap` `{"x","y"}` · `/v1/swipe` `{"x1","y1","x2","y2","ms"}` · `/v1/text` `{"text"}` · `/v1/key` `{"key": "BACK"\|4}` | `{"ok":true,"via":"uidump"\|"input"}` | uidump; si no, `input` |
| `POST /v1/install` | el APK como cuerpo (hasta 128 MiB) | `cmd package install -r -t -S <tamaño>` por stdin: ningún fichero temporal |
| `POST /v1/launch` `{"package"}` | abre la actividad de LAUNCHER | `cmd package resolve-activity` + `am start -W` |
| `GET /v1/logs?buffer=main\|system\|crash\|events\|all\|phoned&lines=N` | texto | `logcat -d`; `phoned`: los últimos 256 KiB del propio agente |
| `GET /v1/identity` | `serial`, `android_id`, `device_name`, `adb_secure`, `adb_keys` (cuántas), `ssaid_userkey_sha256` (huella), `ssaid` (paquete → SSAID) | para comprobar clones; la clave de SSAID nunca sale, solo su huella |

Ejemplo por el daemon (`phone.sh api` lo envuelve):

```sh
phone.sh api 1 GET /v1/health
phone.sh api 1 GET '/v1/screen?encoding=base64' > pantalla.png
printf '{"x":360,"y":640}' > tap.json && phone.sh api 1 POST /v1/tap tap.json
base64 < termux.apk | tr -d '\n' > apk.b64 && phone.sh api 1 POST '/v1/install?encoding=base64' apk.b64
```

**Sin shell.** No hay ruta para ejecutar órdenes: cada operación valida sus
argumentos (coordenadas, nombres de tecla y de paquete por expresión regular,
texto de una línea, APK con la firma ZIP) y el guion de cada una es fijo. Una
shell en la API sería root en Android por un puerto que hoy no autentica, es
decir, lo mismo que `allow_exec` sin su control en el daemon; quien la necesite
usa `kling exec` + `android-sh`, que sí lo exige.

## Modelo de amenazas: quién puede hablar con el 8091

La API da el control del teléfono (instalar un APK es ejecutar código en él).
No autentica, igual que el agente del 8080 (que sirve `exec` como root en la VM):
la confianza es la de la red de la VM.

| Quién | ¿Llega? | Por qué |
|---|---|---|
| El daemon (`POST /machines/{ref}/guest`) | sí, si la máquina declara el puerto en `kling.ports` | es el camino previsto; quien llama al daemon ya puede borrar, congelar o clonar la máquina |
| Un reenvío del Mac (vz) | sí, en `127.0.0.1` del Mac, si está en `kling.ports` | como adb: cualquier proceso local del usuario del Mac. `kling.ports=5555` sin el 8091 lo cierra (el proxy del daemon deja de llegar también) |
| El anfitrión Linux | cualquier proceso del anfitrión alcanza la IP del tap | lo mismo que para el 8080; el anfitrión es de confianza |
| Otras microVMs | no | el FORWARD entre máquinas está cerrado; una **arista de grafo** (`link`) al 8091 sí llegaría, y es a propósito: una arista es una autorización (SECURITY.md §15), así que declararla es dar el control del teléfono a ese nodo |
| Android (sus apps) | no | con `veth`, `DROP` de todo lo que entra por `kandroid0`; con `isolated`, no hay ruta; el 8091 no se reenvía a Android |
| El gateway de kindling | no | reenvía MCP al 8080 y corta las rutas de control; no conoce el 8091 |

Pendiente para una arista de grafo real: un token por arista (el daemon ya
entrega credenciales por arista) o un puerto de solo lectura (health, screen,
tree) separado del de control.

## Identidad por clon (#92)

La entrega el núcleo tras restaurar: `kling machine secret <m> -hooks` pone el
documento en MMDS y lanza los ganchos; `printf '{}' | kling machine secret <m>`
lo vacía después y, como el gancho salió con 0, el daemon levanta `has_secrets`.

```json
{"phone": {"android_id": "<16 hex>", "name": "phone-3", "serial": "<6-20 alfanuméricos>",
           "adb_keys": ["<línea de adbkey.pub>"], "ssaid": "regen", "ssaid_key": "<64 hex>"}}
```

Solo `android_id` es obligatorio; sin `serial` ni `ssaid_key` se sacan del CRNG
del kernel ya resembrado. Qué hace `kling-phoned identity`, en orden:

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

