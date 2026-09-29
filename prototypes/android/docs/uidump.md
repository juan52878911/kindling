# uidump: la jerarquía de la interfaz en ~20 ms

`uiautomator dump` tarda ~1,9 s dentro de Android: arranca una JVM
(`app_process`), conecta UiAutomation, espera **1 s** de interfaz quieta
(`waitForIdle(1000, 10000)` en `DumpCommand`) y suelta la conexión. `uidump` es
un servidor residente que hace el arranque una vez y mantiene UiAutomation
mientras se use, más un cliente en la VM que le pide el XML por un socket unix.

**El XML es el mismo, byte a byte**: el servidor llama por reflexión al
`AccessibilityNodeInfoDumper` de `/system/framework/uiautomator.jar` (su
`dumpNodeRec`, con el serializador en memoria en vez de un fichero), con las
mismas banderas, la misma raíz y el mismo tamaño de pantalla que el
`DumpCommand` de Android 13 (desensamblado con `dexdump` del propio
dispositivo: `getRealSize`, no `getSize`).

## Cómo se usa

```
kling exec <m> -- uidump                        > ui.xml   # = uiautomator dump, sin esperar 1 s
kling exec <m> -- uidump dump --idle 300        > ui.xml   # espera 300 ms de interfaz quieta
kling exec <m> -- uidump dump --compressed      > ui.xml   # = uiautomator dump --compressed
kling exec <m> -- uidump dump --windows         > ui.xml   # = uiautomator dump --windows
kling exec <m> -- uidump tap 360 640
kling exec <m> -- uidump swipe 360 1000 360 300 [MS]
kling exec <m> -- uidump text 'hola'
kling exec <m> -- uidump key BACK               # 4, BACK o KEYCODE_BACK
kling exec <m> -- uidump status | start | stop | connect | release
```

El XML sale por la salida estándar (no a un fichero de Android). Un error del
servidor va a stderr con `ERROR:` y código 1; si Android no corre, 125.

- **Sin `--idle` no se espera nada**: el dump es la interfaz de ese instante,
  también a mitad de una animación o un desplazamiento. Tras una acción que
  cambia de pantalla, `dump --idle 200..300` da lo mismo que `uiautomator
  dump` (comprobado tras un swipe en Ajustes).
- Las acciones vuelven cuando el evento llega a la ventana, no cuando la app
  termina de procesarlo (ver "Entrada"): justo tras `key HOME`, los primeros
  dumps aún pueden ser de la app anterior.

## Cifras (MacBook Air M4, vz, `-mem 1536 -cpus 2 -cpu-pct 200`, 2026-09-27)

De extremo a extremo desde el Mac (`kling exec` incluido), 50 dumps seguidos,
con el Mac bajo presión de memoria (swap 7-8 GiB de otros agentes):

| | `uiautomator dump` (10) | `uidump` (50) | mismo XML |
|---|---|---|---|
| inicio (launcher), 25 nodos | p50 1973, p95 2045 ms | **p50 19, p95 36 ms** | byte a byte |
| Ajustes abierto, 63 nodos | p50 1946, p95 2260 ms | **p50 19, p95 24 ms** | byte a byte |
| `--compressed` | — | p50 18 ms | byte a byte |
| `--windows` (3 ventanas) | — | p50 21 ms | salvo el `id` de ventana, que cambia en cada conexión (también entre dos `uiautomator`) |

Otras corridas en la VM fría: 16-21 ms de p50, p95 ≤ 30 ms.

- Desglose: `kling exec -- true` p50 10 ms; `uidump ping` (nsenter + nc, sin
  tocar UiAutomation) 13 ms; `uidump dump` 19-23 ms. Dentro de la VM, 500 dumps
  seguidos: 10-13 ms de media, 0 fallos.
- **Restaurar un clon → primer dump correcto: 0,62 s** (dorado guardado con el
  servidor vivo y UiAutomation tomado; `run -from` 0,58 s + dump 37 ms). La
  primera restauración de un dorado recién guardado: 1,5-1,65 s (lee el estado
  del disco, igual que con uiautomator). Con `uiautomator` eran 2,7 s (README).
  50 dumps en el clon: p50 15, p95 16 ms.
- `kling pause` + `thaw` → dump: 17 ms. El servidor ni se entera.
- Tras `release` (o tras 30 s sin peticiones) el siguiente dump reconecta:
  30-70 ms de extremo a extremo (5-8 ms la conexión en sí).
- Servidor muerto (`kill -9`) → primer dump: 148 ms (el cliente lo relanza;
  arrancar la JVM de `app_process` cuesta ~130 ms: el grueso de los 1,9 s de
  uiautomator es la espera de quietud). Parado con `uidump stop` → 150 ms.
- **Memoria**: PSS del servidor 39-57 MB (RSS ~115-130 MB, casi todo código
  compartido del boot image). No sale de zygote, así que no comparte su montón.
- Entrada, 20 seguidas: `uidump tap` p50 14 ms frente a `input tap` 33 ms;
  `key` ~20 ms; `text wifi` en el buscador de Ajustes 217 ms (busca a cada
  tecla). En Android 13 `input` ya no es una JVM (`cmd input`), así que la
  ganancia aquí es pequeña: la entrada está por comodidad, no por velocidad.

Resultados crudos en `results/uidump/` (no se versionan): `bench.sh` los deja ahí.

## Cómo funciona

```
Mac: kling exec <m> -- uidump dump
  VM: uidump (bash) ── nsenter (espacios del init de Android, sin entorno de zygote)
        └─ /system/bin/nc -U /dev/socket/kindling-uidump   (toybox)
  Android: kindling-uidump (app_process, servicio de init, root)
        UiAutomation ── AccessibilityNodeInfoDumper.dumpNodeRec ── XML
```

- **Servidor** (`uidump/src/kindling/uidump/Server.java`, un dex de 16 KB):
  `app_process /system/bin --nice-name=kindling-uidump kindling.uidump.Server`
  con `CLASSPATH=uiautomator.jar:kindling-uidump.dex`. UiAutomation por
  reflexión (`new UiAutomation(Looper, new UiAutomationConnection())` +
  `connect()`, como `UiAutomationShellWrapper`); en `app_process` no hay lista
  negra de API ocultas. Socket unix de fichero en `/dev/socket`, 0600 de root:
  ni las apps ni `shell` llegan a él (un TCP en `lo` o un socket abstracto sí
  serían alcanzables por cualquier app). Una petición por conexión, en serie.
- Antes de cada dump se vacía la caché de nodos de la conexión
  (`AccessibilityInteractionClient.clearCache(connId)`), como si fuera un
  uiautomator recién conectado: nunca devuelve un árbol viejo.
- **UiAutomation es uno solo en Android.** Mientras el servidor lo tiene,
  `uiautomator dump` y las pruebas de instrumentación fallan. Por eso no lo toma
  hasta la primera petición y lo suelta tras 30 s sin peticiones
  (`--release-after-ms`, 0 = nunca) o con `uidump release`. `uidump connect` lo
  toma sin volcar (antes de guardar un dorado).
- **Servicio de init** (`uidump/kindling-uidump.rc` en `/system/etc/init/`):
  arranca con `sys.boot_completed=1`, así un dorado guardado después ya trae la
  JVM caliente; init lo relanza si muere.
- **Cliente** (`uidump/uidump`, en `/usr/local/bin` de la VM): encuentra el init
  de Android (lo guarda en `/run/kindling-uidump.pid`), y si el socket no
  responde mata cualquier servidor roto, lo arranca (`ctl.stop` + `ctl.start`
  del servicio; sin él, `app_process` a mano vía `android-sh`) y repite.
- **Entrada**: por `InputManager.injectInputEvent` en modo `WAIT_FOR_RESULT`,
  no por UiAutomation (que usa `WAIT_FOR_FINISH`: espera a que la app termine
  de procesarlo; con render por CPU un tap costaba ~1 s). No necesita
  UiAutomation, así que funciona también tras `release`. `text` inyecta las
  teclas asíncronas y espera solo la última.

## Construir y probar

En la imagen: `build-image.sh` lo compila e instala (bloque `# ── uidump ──`).
Necesita un JDK 17 en el constructor (`apt-get install
openjdk-17-jdk-headless unzip`); `UIDUMP=auto` (por defecto) lo salta con un
aviso si no hay `javac`, `UIDUMP=1` lo exige, `UIDUMP=0` lo quita.
`uidump/build.sh` baja `android.jar` (plataforma 33, `platform-33-ext5_r01.zip`)
y r8/d8 8.13.24 de Google y comprueba su sha256; el dex sale reproducible.

Sin reconstruir la imagen, en una VM viva:

```
prototypes/android/uidump/build.sh /tmp/kindling-uidump.dex        # en Linux
kling cp /tmp/kindling-uidump.dex <m>:/android/system/framework/kindling-uidump.dex
kling cp prototypes/android/uidump/uidump <m>:/usr/local/bin/uidump
kling exec <m> -- uidump start       # sin el .rc lo arranca a mano
KLING_HOST=... prototypes/android/uidump/bench.sh <m> 50 etiqueta
```

(Copiar también `kindling-uidump.rc` a `/android/system/etc/init/` y reiniciar
Android — `kill -9 $(android-sh --pid)`, el lanzador lo relanza — prueba el
servicio de init; así se validó aquí. La imagen completa no se reconstruyó: el
bloque de `build-image.sh` se ejecutó aislado sobre un árbol falso en Lima y
dejó los mismos ficheros y el mismo dex.)

## Limitaciones

- Mientras el servidor tiene UiAutomation, `uiautomator` y la instrumentación
  fallan ("already registered"): `uidump release` antes, o esperar 30 s.
- Sin `--idle`, el dump no espera a que la interfaz se calme (es lo que lo hace
  rápido). Quien necesite el estado final tras una acción pide `--idle`.
- Solo Android 13 probado (Redroid 13 arm64). Lo interno que se usa por
  reflexión (`UiAutomation(Looper, IUiAutomationConnection)`, `connect()`,
  `AccessibilityNodeInfoDumper.dumpNodeRec`, `DisplayManagerGlobal`,
  `InputManager.injectInputEvent(ev, mode)`) es de Android 13; en otra versión
  hay que comprobarlo y volver a comparar el XML (`bench.sh` lo hace).
- `text` usa el mapa de teclado virtual: sin acentos ni emoji (como `input text`).
- Una sola petición a la vez; una segunda espera a la primera.

## Lo que costó encontrar

- **El `LocalSocket` de escucha tiene que tener una referencia viva.**
  `LocalServerSocket(FileDescriptor)` comparte el descriptor y el finalizador
  del `LocalSocket` lo cierra en el primer GC: a los pocos dumps `accept` daba
  EBADF, el cliente creía muerto al servidor y arrancaba otro, que no podía
  tomar UiAutomation porque aún lo tenía el primero.
- **Nada de Android fuera de su espacio de PIDs.** Un `nsenter` sin `--pid`
  lanzó `app_process` con el `/proc` de Android pero fuera de su espacio: ART
  no encuentra `/proc/self/exe`, aborta, debuggerd no puede con él, y la
  tormenta de `crash_dump64` acabó en el OOM del invitado y se llevó a Android
  (dos veces).
- **init tarda 5 s en relanzar un servicio muerto** (`restart_period`, mínimo
  5) y `ctl.restart` no se salta esa espera; `ctl.stop` + `ctl.start` sí
  (5,1 s → 0,15 s).
- `--windows` justo después de activar `FLAG_RETRIEVE_INTERACTIVE_WINDOWS`
  devuelve 0 ventanas y luego 1 de 3: system_server las rellena en diferido. Se
  espera a que dejen de llegar eventos (100 ms) solo cuando cambia la bandera.
- La primera VM de esta prueba mostró la corrupción de RAM del invitado que ya
  apunta el README: `pgrep` falló con "unsupported version 0 of Verneed record"
  y tras `drop_caches` su md5 cambió al correcto (una página de la caché con
  ceros; en arm64 el 0 es una instrucción indefinida, de ahí los SIGILL). Fue
  con el Mac en swap 7+ GiB y antes de arrancar ningún servidor.
