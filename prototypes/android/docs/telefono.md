# Teléfonos para usar y ver: `phone.sh`

Sobre la fase 0 (Android 13 de Redroid en una microVM `vz`), esto deja un
teléfono **usable desde el Mac** con adb, uno o muchos, rápido de sacar del
dorado y de reanudar, cada uno con su identidad. Medido en el MacBook Air M4
de 16 GiB el 2026-09-27, con el Mac muy cargado (swap de 8–12 GiB ocupada por
otros agentes, `kern.memorystatus_level` entre 31 y 70).

```sh
cd prototypes/android
./phone.sh up -n 2        # dos teléfonos (hace el dorado la primera vez)
./phone.sh ls             # nombre, estado, adb y vnc en 127.0.0.1
./phone.sh adb 1 shell    # adb normal contra phone-1
./phone.sh view 1         # scrcpy si está; si no, una captura en Vista Previa
./phone.sh pause 1        # pausado en RAM; ./phone.sh resume 1 en ~10 ms
./phone.sh pool 3         # tres pausados listos para resume
./phone.sh rm -a
```

## 1. Red: veth, no la red compartida

**Decisión: (b), un enlace privado Android↔VM (par veth) con NAT en la VM,
ahora el valor por defecto (`ANDROID_NET=veth`).** La red compartida no vale
tal cual:

| Prueba (misma imagen, misma VM) | `shared` | `veth` |
|---|---|---|
| reglas de enrutamiento de la VM tras arrancar Android | netd borra `32766: from all lookup main` y añade `32000: from all unreachable` (tablas por fwmark de netd) | intactas: `local`, `main`, `default` |
| `adb connect 127.0.0.1:<p>` desde el Mac | `failed to connect`; `adb: device offline` (adbd sí escuchaba en `*:5555`: las respuestas de la VM ya no tenían ruta) | conecta; `ro.product.model=redroid13_arm64_only` |
| netd y su `eth0` | toca el `eth0` de la VM (le cambió la IPv6; el coordinador lo vio) | adopta su propio `eth0` (`10.88.0.2/30`, tabla `eth0` de netd), como con Docker |
| egress (`-egress internet`) | — | `nc 1.1.1.1 443` ok, `GET http://1.1.1.1/` → `301`, DNS resuelve |
| MMDS (169.254.169.254) y kling-guest (:8080) desde Android | alcanzables (misma red) | **bloqueados** (timeout) |

Que la prueba del coordinador funcionara con `shared` y la mía no es de
esperar: depende de si netd ya había reescrito las reglas cuando se probó.
Con veth no hay carrera.

Cómo va (`image/android-launch.sh`, `red_veth`):

- un espacio de red con nombre `/run/netns/kandroid`; `eth0` dentro (un
  extremo del veth, `10.88.0.2/30`, ruta por defecto `10.88.0.1`), `kandroid0`
  fuera. Está configurado **antes** de `/init`, como lo deja Docker: el
  `ipconfigstore` de Redroid lee esa IP en `post-fs-data` y la guarda como
  estática para EthernetTracker;
- en la VM (iptables-legacy: el kernel no trae nf_tables): DNAT de 5555 y
  5900 que llegan por el `eth0` de la VM hacia Android, MASQUERADE de lo que
  sale de Android (sale con la IP de la VM, así que **la política de egress de
  kindling sigue mandando**), y Android no puede hablar con la VM ni con MMDS;
- DNS: el de la VM (`androidboot.redroid_net_dns1`);
- idempotente (el bucle del entrypoint relanza el lanzador) y sin nada que
  rehacer tras restaurar: todo vive en la RAM del dorado;
- si falta veth o iptables, avisa y cae a la red aislada (solo `lo`), que es lo
  de antes.

Lo que pide fuera de mis ficheros: **`CONFIG_VETH=y`** en
`kernel/config-android` (una línea; kernel nuevo `sha256 46570bc6…`, 447 s en
Lima) e **`iptables`** en la base (`build-image.sh`, `BASE_PKGS`; hay que
borrar `$WORK/images/android-base.ext4` para que se reconstruya).

Validado con veth: restaurar (todos los clones de aquí), `kling exec` y
`kling save` (el dorado se guarda con la red veth dentro), egress a internet
desde un clone, pause/thaw, y 20 min de uso (sección 6). `sandbox fork`:
sección 6.

## 2. `phone.sh`

bash 3.2; perl (del sistema) para leer `kling ps -json`. Arranca su propio
daemon privado (`PHONE_ROOT`, por defecto `~/.kindling-android-telefono`, con
`images/vmlinux` y la imagen `android13`) si no hay uno; `daemon stop` lo para.

- `up [-n N]`: `kling run -from` del dorado, espera a que `adb shell true`
  conteste, inyecta la identidad (sección 4) e imprime puertos y tiempos. Si
  no hay dorado lo crea. No arranca otro teléfono mientras
  `kern.memorystatus_level < PHONE_MIN_MEMLEVEL` (35 por defecto).
- **Los puertos no se guardan nunca**: cada orden los lee de `forwards` en
  `kling ps -json`. Medido: tras pause/thaw el puerto del Mac **no** cambia;
  tras restaurar (cada clone nuevo) sí.
- `adb <tel>` hace `adb connect` y reintenta hasta que contesta; `pause` y
  `rm` hacen `adb disconnect` para que `adb devices` no se llene de
  `offline`.
- `pool N`: crea teléfonos y los pausa hasta tener N pausados.
- `golden rebuild`: borra el dorado y lo rehace.
- **`kling run -from` pone egress `none` si no se le pasa `-egress`**, aunque
  el dorado diga `internet` (medido: el clone salía `egress=none`). `phone.sh`
  le pasa el del dorado (`kling template inspect -json`). Es un hallazgo del
  CLI: la plantilla guarda el egress pero el cliente siempre manda uno.
- `-ttl 0` no quita el TTL (significa "el de la configuración", 10 min, y el
  teléfono se congelaría solo): se usa `-ttl 8760h`.

### Ver la pantalla

- **scrcpy** (sobre adb) si está en el PATH. **No probado**: no está instalado
  aquí y no lo he instalado (`brew install scrcpy`).
- Sin scrcpy: una captura por adb (`screencap -p`) abierta en Vista Previa.
- `view -vnc`: el VNC de Redroid llega al Mac (`RFB 003.008`, seguridad
  "None", `ServerInit 720x1280 name=redroid`), pero:
  1. **Compartir Pantalla no conecta a un VNC sin autenticación**: se queda en
     "Conectando…" para siempre. Además contesta `RFB 003.003`. `phone.sh`
     pone un puente local (perl, en 127.0.0.1) que le ofrece "VNC
     Authentication", acepta cualquier contraseña y habla "None" con Redroid:
     con él la ventana `redroid` se abre;
  2. **pero la imagen llega en negro**: un fotograma Raw completo tiene 36
     bytes no nulos de 3 686 400. El `vncserver` de Redroid falla al importar
     el búfer de la pantalla con render por software (`RfbServer: error
     creating EGLImage: 0x300c`), mientras `screencap` sí ve la pantalla de
     inicio. Arreglarlo es del lado de la imagen (gralloc/minigbm con
     dma-buf, o un servidor VNC que lea de SurfaceFlinger); queda abierto.
  `androidboot.use_redroid_vnc=1` necesita también
  `androidboot.use_redroid_stream=1` (`vncserver.rc`), que además arranca
  `uinputd` (entrada). Los dos van ahora por defecto en `build-image.sh`.

## 3. Dorado bien hecho

`kling save` solo tras `sys.boot_completed=1`, pantalla encendida sin bloqueo
ni animaciones, `init.svc.adbd=running`, y dos comprobaciones nuevas, porque
**con el Mac bajo presión de memoria la caché de páginas del invitado se
corrompe**:

- En dos VMs (una en frío, otra restaurada) `grep` daba `Segmentation fault`,
  `ip` decía `libelf.so.1: invalid ELF header` y procesos nuevos de Android
  morían con `SIGILL ILL_ILLOPC` **siempre en el mismo sitio**
  (`libsfplugin_ccodec.so+0x6a7e8`, el principio de una función). Leído desde
  el invitado, ese trozo del fichero eran **ceros**; en el disco (debugfs en
  el Mac, `dd iflag=direct` desde el invitado: el sha256 del disco coincide)
  es código. `0x00000000` es `udf #0`: el SIGILL. Es lo mismo que el "Abierto"
  del README (SIGILL en apexd). No es balloon/free page reporting: el balloon
  de vz no anuncia `VIRTIO_BALLOON_F_REPORTING`.
- Un dorado guardado así reparte las páginas rotas a **todos** sus clones (el
  primer dorado lo hizo: 5 bibliotecas a ceros en cada clone).

Por eso `phone.sh` antes de guardar hace `drop_caches` y
`android-sh --verify-cache` (compara cada fichero de `system/lib64`,
`system/bin`, `system/framework`, `system/apex`, `vendor/lib64` y
`vendor/bin` leído por la caché con el mismo leído con `O_DIRECT`: 1623
ficheros, 9 s) y mira que `logcat -b crash` no tenga ni un SIGILL ni una caída
de system_server. Si algo falla, no guarda y lo dice. Tras restaurar, si la
identidad no se aplica (`settings` no contesta), `up` enseña el diagnóstico.

Tiempos medidos con `phone.sh` (reloj de pared desde el script, incluye el
CLI). Dos series: la primera con el Mac a nivel ~50 y swap llena; la segunda
(la que va en la sección 6) media hora después, con menos presión:

| | 1.ª serie | 2.ª serie |
|---|---|---|
| arranque en frío → `boot_completed` | 5,5 s (9,8 s con el Mac a nivel 40) | — |
| `--verify-cache` del dorado (1623 ficheros) | 9,1 s | — |
| `kling save` | 1,5–1,9 s | — |
| `kling run -from` | 1,46–2,05 s | 0,67–0,94 s |
| **restaurar → `adb shell true`** | **1,53–2,14 s** | **0,73–0,99 s** |
| identidad (secreto + aplicar + vaciar) | 0,07–0,15 s | 0,08 s |
| `adb exec-out screencap -p` | 0,39–0,41 s | 0,34–0,35 s |
| `kling thaw` de un pausado | 0,01 s | 0,01–0,02 s |
| **thaw → screencap por adb** | **0,44–0,53 s** | **0,36 s** |

`adb shell true` contesta 0,05–0,16 s después de que `kling run -from`
vuelva: el suelo es restaurar la RAM en vz (README).

## 4. Identidad por clon

> Con el núcleo nuevo (capacidad `ready`, sección 8) los pasos 2 y 3 son del
> núcleo: `kling machine secret <m> -hooks` inyecta y ejecuta el gancho de la
> imagen (`/etc/kindling/post-restore.d/10-identity`, que llama a
> `android-sh --identity`), y al vaciar el almacén la marca `has_secrets` se
> levanta: el teléfono vuelve a poder congelarse. Lo de abajo es cómo va con
> kling v0.16, que `phone.sh` sigue soportando.

Todos los clones despiertan con el `android_id` del dorado. El mecanismo de
secretos de kindling es MMDS: `kling machine secret <m>` (stdin) →
`POST /machines/{ref}/mmds` → `kling-vz` lo sirve en `169.254.169.254` solo
en RAM (no viaja en el snapshot) con MMDS v2 (token por PUT). Al inyectarlo el
daemon marca la máquina `has_secrets` y **se niega a guardarla, congelarla o
clonarla** (así el secreto nunca llega a un `mem.file` compartido); pausar sí.

Sin tocar el núcleo Go:

1. `phone.sh` genera en el Mac un `android_id` (16 hex de `/dev/urandom`) y
   el nombre, y los pasa por **stdin** a `kling machine secret` como
   `{"phone":{"android_id":"…","name":"phone-N"}}`;
2. `kling exec <m> -- android-sh --identity`: `android-sh` lee
   `/phone/android_id` y `/phone/name` de MMDS con los builtins de bash
   (`/dev/tcp`, la base no trae curl), los valida y hace
   `settings put secure android_id` y `settings put global device_name`
   dentro de Android;
3. `phone.sh` vacía el almacén (`{}`).

Verificado (sección 6): 3 clones, 3 `android_id` distintos; ninguno aparece
en `kling ps -json`, en `daemon.log`, en los logs de consola de las VMs ni en
los ficheros del dorado (`mem.file` incluido); `kling save` de un teléfono con
identidad se niega.

Lo que no cubre:

- **`ro.serialno`** está vacío en Redroid y es `ro.*` (no se cambia tras el
  arranque); el serial que ve adb es `127.0.0.1:<puerto>`, distinto por clon.
- **Claves de adb**: Redroid va con `ro.adb.secure=0` (sin autenticación), así
  que no hay claves por clon que cambiar. Solo es aceptable porque adb solo
  escucha en 127.0.0.1 del Mac.
- El `android_id` que ven las **apps** (SSAID por app desde Android 8) se
  deriva de una clave de usuario en `settings_ssaid.xml`, igual en todos los
  clones. No lo he cambiado.

## 5. Ficheros

| Fichero | Qué |
|---|---|
| `phone.sh` | lo de arriba |
| `test-phone.sh` | la prueba de la sección 6 |
| `image/android-launch.sh` | `ANDROID_NET=veth` (por defecto), `isolated`, `shared` |
| `image/android-sh` | `--identity` (MMDS) y `--verify-cache` |
| `image/build-image.sh` | por defecto `veth`, `use_redroid_stream=1 use_redroid_vnc=1`, `iptables` en la base |
| `kernel/config-android` | `CONFIG_VETH=y` |

La imagen de estas pruebas es la base reconstruida con `iptables`
(`71-build-glibc-base.sh`, los mismos parámetros que `build-image.sh`) y la
capa de la fase 0 con `android-launch.sh`, `android-sh` y `android.conf`
sustituidos con `debugfs -w` en el Mac: el disco de Lima (5,8 GiB libres) no da
para otra capa entera más su paquete. **`build-image.sh` completo con estos
cambios no se ha ejecutado.**

## 6. Resultados

`PHONE_MIN_MEMLEVEL=45 ./test-phone.sh` (2026-09-27 21:33, dorado hecho con
`PHONE_EGRESS=internet`):

```
raíz /Users/juanbedoya/.kindling-android-telefono · salida /private/tmp/claude-501/tel/run2 · 2026-09-27 21:33:31 · kern.memorystatus_level=50
ok    up -n 2 en 1.99 s
        phone-1	adb 127.0.0.1:62742	vnc 127.0.0.1:62743	(restore 0.94 s, adb listo 0.99 s, identidad 0.08 s)
        phone-2	adb 127.0.0.1:62753	vnc 127.0.0.1:62754	(restore 0.67 s, adb listo 0.73 s, identidad 0.08 s)
        NAME     STATE    ADB              VNC
        phone-1  running  127.0.0.1:62742  127.0.0.1:62743
        phone-2  running  127.0.0.1:62753  127.0.0.1:62754
ok    phone-1: adb 127.0.0.1:62742, boot_completed=1
ok    phone-1: screencap por adb 0.35 s (655099 bytes)
ok    phone-1: android_id cacf52cde3b333e2, device_name phone-1
ok    phone-2: adb 127.0.0.1:62753, boot_completed=1
ok    phone-2: screencap por adb 0.34 s (655099 bytes)
ok    phone-2: android_id 482d6b1240b2d4c2, device_name phone-2
ok    pause phone-1
ok    phone-1 está paused
ok    resume: phone-1	adb 127.0.0.1:62742	vnc 127.0.0.1:62743	(thaw 0.01 s, adb listo 0.07 s)
        puerto adb antes 62742, después 62742
ok    screencap por adb tras resume 0.33 s
ok    ciclo 1: thaw 0.02 s, thaw→screencap por adb 0.36 s
ok    ciclo 2: thaw 0.02 s, thaw→screencap por adb 0.36 s
ok    ciclo 3: thaw 0.01 s, thaw→screencap por adb 0.36 s
ok    kling save phone-2 se niega: checking the guest is serving before freezing... ✓ (serving)
error: machine phone-2 has …
ok    rm phone-2
        phone-2	adb 127.0.0.1:62852	vnc 127.0.0.1:62853	(restore 0.88 s, adb listo 0.93 s, identidad 0.08 s)
ok    phone-2: android_id bdc133570a583a32
ok    3 clones, 3 android_id distintos: cacf52cde3b333e2 482d6b1240b2d4c2 bdc133570a583a32
ok    ningún android_id en kling ps, en los logs del daemon/VMs ni en los ficheros del dorado (442M)
ok    rm -a: no quedan teléfonos
ok    adb devices sin entradas de los teléfonos borrados

PASS (salida en /private/tmp/claude-501/tel/run2)
```

La primera ejecución completa (21:04, con más presión) también pasó; sus
tiempos son la 1.ª serie de la sección 3.

**20 min de uso** de un clon (red veth, egress internet), una ronda cada 30 s
con `adb shell`, `screencap` por adb, `kling exec`, `GET http://1.1.1.1/` desde
Android, PID de system_server y `logcat -b crash`: **40 rondas, 0 fallos**,
el mismo system_server (PID 252) de principio a fin y 0 caídas, con
`kern.memorystatus_level` oscilando entre 32 y 70.

**`sandbox fork`** (`sandbox create -from phone-golden` + `fork -n 1`): 2,84 s;
la copia contesta a adb (`redroid13_arm64_only`), sale a internet (`301`) y
`kling exec` va. Del original solo se comprobó que seguía `running`.

**`pool 2` + `resume 2`**: dos teléfonos pausados; `resume` en 0,01 s, adb en
0,07 s, con su propio `android_id`.

## 7. Lo que queda abierto

(Ver también la sección 8, con el núcleo nuevo.)

- **La corrupción de la caché de páginas bajo presión** (sección 3) es del
  entorno `vz` del Mac, no de estos scripts: `phone.sh` la detecta antes de
  guardar el dorado, pero un clon ya restaurado puede estropearse después.
  `android-sh --verify-cache` sirve para comprobarlo en cualquier VM.
- VNC en negro con render por software; scrcpy sin probar.
- SSAID por app igual en todos los clones.
- `build-image.sh` completo sin ejecutar con estos cambios (sección 5).

## 8. Con el núcleo de "listo" y ganchos (29-09-2026)

Probado en el M4 con los binarios de la rama (CLI, daemon privado, `kling-vz`,
`kling-guest`), kernel y imagen rehechos en Lima (`SLIM=1 VERITY=1
DATA_MODE=overlay`), con el Mac en `kern.memorystatus_level` 33–65 y 4,9 de 6 GiB
de swap.

**Qué hace ahora `phone.sh`.** Si el CLI tiene `-wait-ready` y el daemon la
capacidad `ready` (lo mira una vez con `kling run -h` y `kling info`):

- el dorado arranca con `kling run -wait-ready` (la sonda de la imagen,
  `/etc/kindling/ready`, es `sys.boot_completed=1`) y `kling machine ready` lo
  confirma; `kling save` vuelve a esperar a la sonda antes de pausar. La
  comprobación de la caché, la de salud, la pantalla y la espera a adbd siguen;
- cada clon es `kling run -from -wait-ready` (la copia sale lista en cuanto
  acaba el gancho tras restaurar, que sin identidad en MMDS no hace nada);
- la identidad va por `kling machine secret <m> -hooks` (el gancho
  `10-identity` la aplica y el daemon espera a que salga con 0) y luego `{}`,
  que **levanta la marca de secretos**: el teléfono se congela y descongela;
- no pasa `-cpu-pct`: la receta trae `cpu_pct_per_vcpu: 100` (200 con 2 vCPU).
  `PHONE_CPU_PCT` sigue mandando si se da.

Con kling v0.16 hace lo de antes (sondeo de getprop, `android-sh --identity`
por `kling exec`, `-cpu-pct CPUS×100`, el teléfono marcado).

**Lo que falló y cómo se arregló.**

1. **Android arrancaba, se guardaba y no se podía usar.** El núcleo arranca
   desde el 27-09 con `ipv6.disable=1` (barrera IPv6, SECURITY.md). Sin
   módulo IPv6, el `adbd` de Redroid no escucha nada (solo abre `[::]:5555`:
   `socket_inaddr_any_server` no cae a IPv4) y el `IpClient` del `eth0` de
   Android falla al arrancar IPv6 (`onProvisioningFailure(): 5`,
   ERROR_STARTING_IPV6), reintenta cada ~15 ms y en cada intento borra las
   direcciones del `eth0`: `adb connect` → `device offline`, y un bucle
   gastando CPU. Arreglo en el núcleo: `"guest_ipv6_stack": true` en la
   receta (`build-image.sh` lo escribe) arranca con `ipv6.disable_ipv6=1`, el
   módulo cargado sin direcciones; la barrera del host no cambia. El `eth0` de
   Android vuelve a tener `10.88.0.2/30` (y su link-local, dentro de su espacio
   de red).
2. **`-wait-ready` tras restaurar costaba ~300 ms.** Los ganchos tardan 50 ms,
   pero la espera preguntaba cada 250 ms desde la primera vez. Ahora pregunta a
   los 25 ms y dobla: `run -from -wait-ready` 0,73–0,98 s frente a 0,75–0,82 s
   sin esperar (restaurar en vz: 0,62–0,80 s; el primero tras guardar, 1,5–2,4 s).
3. `test-phone.sh` probaba que un teléfono con identidad **no** se deja
   guardar; con el núcleo nuevo prueba lo contrario (freeze/thaw aceptados,
   `android_id` intacto, sin `has_secrets`, columna `READY`), y olvida en adb
   el puerto viejo antes de congelar (si no, `adb devices` lo enseñaba).
4. `build-image.sh` no cabía en el disco de la VM Lima (4,5 GiB libres): el
   árbol de Android y dos copias de la capa. Ahora borra el árbol antes de
   empaquetar y enlaza las imágenes en el paquete en vez de copiarlas.

**Cifras.**

| | medido |
|---|---|
| dorado: arranque en frío → listo (sonda) | 5,9 s; 28 s la primera vez tras construir la imagen, con el Mac a nivel 43 |
| dorado: `--verify-cache` (1624 ficheros) | 7,0–8,0 s, 0 desajustes |
| `kling save` (espera a la sonda incluida) | 1,6–3,6 s |
| `phone.sh up`: restaurar / adb listo / identidad | 1,8–3,0 s / 1,8–3,1 s / 0,11–0,29 s |
| `cpu_pct` de un clon sin flag | 200 |
| freeze → thaw → listo (teléfono con identidad) | aceptado; thaw + listo 0,70–1,71 s |
| `squeeze` de un clon de 1,5 GiB | footprint 1930 → 1543 MiB, ~400 MiB devueltos, 400 MiB disponibles dentro |
| `uidump` tras `squeeze` / `uiautomator dump` | 16 ms / 2,2 s (una vez, justo tras apretar, `uiautomator` salió `Killed`; no se repitió en 5 intentos) |
| `uidump` (bench, n=50) | p50 16 ms, p95 22 ms; `uiautomator dump` p50 1957 ms; mismos 19 nodos |
| muro local (`wall-mac.sh -local`) | `/api/phones` con salud ok, `/shot` PNG 720×1280 en 0,40 s, `tap` 0,03 s |
| `stress-restore.sh -modes reads` 10×10 pasadas con verity | 100 pasadas, 0 lecturas malas, 0 líneas de verity; `dmsetup status` = `V` |
| admisión con los valores por defecto | admite (swap al 10 % de lo que puede crecer, 56 GiB libres) |
| `KLING_MAX_SWAP_PCT=5` / `KLING_MIN_FREE_DISK_MIB=900000` | `507` / `503`, con el motivo y qué hacer |
| `test-phone.sh` | PASS (3 `android_id` distintos, ninguno en `ps`, logs ni dorado) |
| `fase0.sh -clones 3` | 5 de 6: el 2 (restaurar → dump ≤ 3 s) dio p50 3,45 s con 3 clones vivos (6,2 GiB) y el Mac a nivel 36–64; restaurar en sí fue 0,6–0,8 s y `uiautomator dump` son 1,94 s de suelo |

Abierto: el criterio 2 de la fase 0 queda justo por encima con el Mac cargado
(la vez anterior, 2,7 s); con `uidump` en vez de `uiautomator dump` sería
~1 s.
