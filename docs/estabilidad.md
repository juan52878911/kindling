# Estabilidad y determinismo

Auditoría del 26–27 de agosto de 2026, hecha **sobre el sistema vivo** (`fc-test`,
Intel i3-3220, KVM anidado sobre Proxmox, 4 GB). Cada hallazgo viene de una
observación, no de leer código. Lo que no pude medir va marcado.

---

## 1. La causa raíz: los arreglos no llegaban a lo que corre

Antes de cualquier fallo concreto, el problema de fondo era de **convergencia**.
Había tres líneas divergentes:

| Línea | Qué llevaba |
|---|---|
| `origin/main` | 1 commit: siete supuestos del entorno |
| `claude/plan-stage-2-impl-fa999b` | 23 commits: imágenes en capas, MCP de Python, navegador compartido — **y era lo que corría en el laboratorio** |
| `fix/robustez-snapshots` | 6 arreglos: segador, guardián de memoria, `-replace`, `mcp health`, TSC |

Lo que se ejecutaba **no contenía ningún arreglo**. Cada corrección aterrizaba
donde no se estaba ejecutando, así que el comportamiento observado nunca mejoraba
por mucho que se arreglara.

Los conflictos eran todos **aditivos** —cada lado añadió algo distinto en el mismo
sitio— y se resolvieron conservando ambos. Dos uniones se comieron la llave de
cierre de la función anterior; repuestas.

> **Regla que sale de aquí:** antes de desplegar, comprobar **qué versión corre el
> destino**. Un `make deploy` desde `main` degradó el laboratorio de una rama 23
> commits por delante, y el daemon dejó de encontrar sus imágenes en capas.

---

## 2. El determinismo: por qué el mismo comando daba resultados distintos

### El fallo

`kling mcp import` hace una danza cuidadosa antes de congelar un dorado:

1. arrancar la microVM,
2. esperar a que el puerto abra,
3. comprobar que el servidor no instala nada en caliente,
4. **resetear el estado post-handshake**,
5. congelar.

**`kling commit` a secas no hacía nada de eso** — y es el camino documentado para
crear dorados a mano.

El resultado es un snapshot que **restaura perfectamente en 26 ms y luego no
contesta**. El fallo no aparece al congelar: aparece al despertar, minutos u horas
después, con un `tool did not start listening` que no menciona el commit. Ese
desfase entre la causa y el síntoma es lo que hacía que el sistema pareciera
aleatorio.

### El arreglo

`commit` exige ahora que el invitado sirva (`-wait`, 60 s por defecto) y pide el
`/reset` del puente para no congelar sesiones abiertas. Si no sirve, **se niega** y
explica la cadena entera: qué pasa ahora, qué pasaría al despertar, cómo
diagnosticarlo, y cómo saltárselo con `-force`.

### Medido después

| | |
|---|---|
| Commit inmediato tras arrancar | rechazo, `exit 1` |
| Commit tras esperar | dorado que responde `HTTP 200` |
| Tres llamadas seguidas | 8,20 s · 7,89 s · 7,88 s |

**Consistente**, que es lo que se buscaba. De esos ~7,9 s, sólo **26 ms** son la
restauración de la microVM; el resto es node arrancando dentro. Ver §4.

---

## 3. Los seis fallos de robustez

Todos observados, todos con test que se pone rojo al romper el código.

| # | Síntoma observado | Causa |
|---|---|---|
| 1 | Una máquina reintentando congelarse **cada 10 s durante 260 h** | El segador no distinguía el fallo estructural (socket desaparecido) del transitorio, y nunca se rendía |
| 2 | **Nueve máquinas `failed`** acumuladas en 21 h | Nadie las recogía; `failed` es terminal, así que sólo eran basura |
| 3 | Arranque rechazado con **3,4 GB reclamables** | El guardián descartaba `MemAvailable` entero asumiendo que toda la caché eran `mem.file` vivos. Con **una** microVM viva eso era falso |
| 4 | Snapshot caducado imposible de reemplazar | `kling commit` no tenía forma de forzar, y los dorados se invalidan en cada reinicio |
| 5 | **Nueve servicios caídos 26 h** con `status` diciendo «✓ 9» | `services: ✓ 9` informaba del inventario y se leía como salud |
| 6 | `Could not set TSC scaling` | Los snapshots quedan atados a la frecuencia del TSC del anfitrión; un reinicio los invalida **todos a la vez** |

### Lo que se decidió NO hacer

Detectar el problema del TSC **antes** de la primera petición. La vía barata
—grabar el `boot_id` del host y compararlo— da **falsos positivos** en anfitriones
con TSC scaling por hardware, donde los snapshots sí restauran tras reiniciar.
Convertir una heurística en bloqueo habría cambiado un fallo tardío por uno
prematuro.

---

## 4. El despertar, descompuesto

Con el dorado congelado **con un hijo caliente sin ligar** (ver §3), medido en el
laboratorio sobre `sequentialthinking`:

| | Antes | Ahora |
|---|---|---|
| Despertar por el gateway | 7,88–8,20 s | **4,76–5,32 s** |
| Del cual, el invitado abre el puerto | ~7,8 s | **~0,3 s** |

El lado del invitado —que era el objetivo— es **26× más rápido**. El coste del
arranque del runtime se movió del despertar al momento de congelar: `kling commit`
pasó de ~6 s a ~14 s, y el dorado de 39 MB a 120 MB de memoria. Es el intercambio
correcto: se paga una vez al construir, no en cada despertar.

### Y aparece otro cuello de botella, mayor

Midiendo `kling run -from` directamente, sin gateway:

| | |
|---|---|
| `kling run -from` completo | **4.260–4.302 ms** |
| El invitado abre el puerto, después | +260–320 ms |
| Restauración de Firecracker | **28–31 ms** |

O sea: de los ~4,8 s del despertar, **~4,3 s son el camino de creación de máquina de
kling** —jail, red, cgroup, disco, contabilidad—, no Firecracker (31 ms) y ya no el
invitado (0,3 s).

Ese es el siguiente objetivo, y es donde más hay que ganar. Sin medirlo por dentro
no se puede decir qué parte de esos 4,3 s es cada cosa.

### Un test que falla en Linux y nadie ve

`TestLiveVMsEncuentraLaMaquinaPorSuSocket` falla en Linux y **ya fallaba antes** de
esta tanda. Sólo corre en Linux (`t.Skip` en macOS), y como el desarrollo es en
macOS, nadie lo ve fallar. No hay Go en las máquinas Linux; se ejercita compilando
el binario de tests en cruzado:

```sh
GOOS=linux GOARCH=amd64 go test -c -o /tmp/m.test ./internal/machine/
scp /tmp/m.test lab:/tmp/ && ssh lab '/tmp/m.test -test.count=1'
```

### La imagen más grande no es reproducible

`playwright` no tiene receta (`RECIPE: no`): es una imagen preconstruida heredada.
Es la única que no se puede rehacer de forma determinista, y es la más grande con
diferencia.

---

## 5. Motor de navegador: la evaluación, medida

Se pedía evaluar «WebView en lugar de un Chromium completo». La conclusión llegó
por un camino distinto al esperado, así que va entera.

### «WebView» no existe dentro de estas microVMs

WebView significa un motor embebido **que aporta el sistema operativo**: WKWebView
en macOS, WebView2 en Windows (que por dentro es Chromium), WebKitGTK en escritorio
Linux. Las microVMs son **Alpine mínimo**: no hay ningún WebView del sistema que
embeber. Y los servidores MCP de navegador hablan **CDP**, que WebKitGTK no habla,
así que usarlo obligaría a escribir un adaptador.

Pero el objetivo detrás de la pregunta —gastar mucho menos— sí se cumple, con otro
binario.

### Dónde está el peso, medido

Desglose de los 883 MB de la imagen `playwright`:

| | |
|---|---|
| `/usr/lib/chromium` | 313 M |
| **`libLLVM.so`** | **183 M** |
| `libgallium.so` | 42 M |
| `libx265.so` | 21 M |
| `libavcodec.so` | 20 M |
| `libaom.so` | 8 M |
| `libgtk-3.so` | 7 M |

Unos **280 MB son pila gráfica y códecs de vídeo**, más GTK —un toolkit de
interfaz— dentro de un navegador headless. Un scraper no toca nada de eso.

**Pero no se pueden quitar por selección de paquetes.** Medido en Alpine 3.21:

| Combinación | Instalado |
|---|---|
| chromium + swiftshader + fuentes (actual) | 745 MiB |
| sin swiftshader | 740 MiB |
| sólo `chromium` | **720 MiB** |

Quitar swiftshader ahorra **5 MiB**. LLVM, Mesa y los códecs son dependencias
duras del paquete `chromium` de Alpine.

### El binario que sí cambia las cosas

`chrome-headless-shell` es el Chromium que Google construye para automatización, sin
capa de interfaz. **Habla el mismo CDP, así que ningún cliente cambia.**

Comparación cara a cara, mismo script, mismos tres sitios reales
(`example.com`, la primera página web del CERN, y el RFC 2324):

| | Alpine + `chromium` | glibc + `headless-shell` |
|---|---|---|
| Imagen total | **986,8 MB** | **637 MB** |
| Arranque (3 pasadas) | 369 · 337 · 395 ms | **107 · 97 · 113 ms** |
| example.com | 449 ms | **147 ms** |
| CERN | 1047 ms | **962 ms** |
| RFC 2324 | 1890 ms | **853 ms** |
| Texto extraído | 129 / 983 / 19601 car. | **idéntico** |

Gana en las dos dimensiones: **35% menos disco y 3,4× más rápido al arrancar**, con
poca varianza en las tres pasadas. Y los caracteres extraídos coinciden **exactos**,
que es lo que prueba que el scraping es equivalente y no una versión degradada.

### El bloqueo, nombrado con precisión

`chrome-headless-shell` es **glibc**; las microVMs son Alpine (**musl**).

Probado: con `gcompat` y 20 bibliotecas de compatibilidad **no arranca** — quedan 52
bibliotecas sin resolver. No es viable.

Todas las bases de kling salen del *minirootfs* de Alpine
(`scripts/70-build-minimal-image.sh`). Usar `headless-shell` exige **una familia de
base glibc** —un script hermano que traiga un rootfs de Debian— y que
`80-mcp-image.sh` sepa elegir motor y escribir el `browser.json` que corresponda.

### Lo que ya está resuelto

El puente **ya es agnóstico al motor**. `cmd/kling-bridge/browser.go` lee
`/etc/kling/browser.json` con `sidecar`, `ready_url` y `session_args`, y su propio
comentario lo dice: *«no sabe (ni le importa) que es Chromium»*. La infraestructura
para tener dos motores y cambiar el defecto **existe**; lo que falta es la base
glibc debajo.

---

## 6. Prueba de estrés (27-08-2026)

Informe visual: `docs/prueba-estres.html`. Lo esencial, todo medido sobre `fc-test`:

### Densidad

| microVMs | PSS total | por réplica | libre | `run -from` |
|---|---|---|---|---|
| 1 | 12 MiB | 12,0 | 224 MB | 4.390 ms |
| 40 | 630 MiB | 15,8 | 113 MB | 4.494 ms |
| 80 | 1.606 MiB | 20,1 | 100 MB | 4.721 ms |
| 120 | 2.572 MiB | 21,4 | 104 MB | 4.974 ms |
| 140 | 2.785 MiB | 19,9 | 103 MB | 5.120 ms |
| **143** | — | — | — | **rechazado** |

**142 microVMs simultáneas en 3,9 GB.** La memoria libre no baja mientras el PSS
sube: la copia-en-escritura comparte las páginas del dorado. El rechazo en la 143 es
determinista y explicado (pide 64 MiB, quedan 424, 384 reservados al anfitrión). Ni
un OOM ni una caída en 142 intentos.

### Carga concurrente

| Concurrentes | Éxito | p50 | p95 |
|---|---|---|---|
| 1 | 1/1 | 4,82 s | 4,82 s |
| 5 | 5/5 | 11,16 s | 11,16 s |
| 10 | 10/10 | 21,78 s | 21,96 s |
| 20 | 20/20 | 44,08 s | 44,29 s |

**100% de éxito, y latencia lineal.** ~2,2 s por petición añadida con el anfitrión al
80% ocioso.

> **Aviso: la conclusión que saqué aquí era errónea.** Interpreté esta linealidad como
> serialización del camino de creación de máquina. No lo era: las 20 peticiones iban al
> MISMO servicio, y el gateway lo serializa a propósito. El cuello real está en la §7,
> junto con los números tras arreglarlo — estos de arriba son los de ANTES.

### Coste por servicio

**120 MB de disco** cada uno (aparente 769 MB; los ficheros son dispersos). A 150
servicios son ~18 GB. **El hijo caliente triplicó esto**: antes eran 39 MB. Bajó el
despertar de 7,9 s a 4,8 s a cambio de 12 GB más a escala de 150. Debería ser opcional.

### Por dónde seguir

1. ~~Paralelizar la creación de máquina~~ — **descartado**: ya era paralelo. El
   coste real era rehashear el dorado en cada instanciación. Ver §7.
2. **Hacer opcional el hijo caliente** — quien tenga 150 servicios querrá elegir.
3. **Sondear la salud sola** — diez servicios siguen con `never probed`.
4. Entrada duplicada en el catálogo tras `-replace`; se cura al reiniciar el daemon,
   sin causa identificada.

> **Un error de método que cometí midiendo.** Un bucle de sondeo `nc` sin pausa
> completa 400 iteraciones fallidas en ~300 ms, así que reporté «puerto listo a los
> 260 ms» cuando el puerto no había abierto nunca. Un bucle que se agota **no es una
> medición**: hay que distinguir "encontré la condición" de "se me acabaron los
> intentos".

---

## 7. El verdadero cuello: verificar lo inmutable 142 veces

Fui a paralelizar la creación de máquina, que en §6 parecía serializada. **No lo
estaba**, y el camino hasta descubrirlo importa más que el arreglo.

### Lo que la primera medición no decía

Las 20 peticiones concurrentes de §6 iban **al mismo servicio**, y el gateway
serializa por servicio **a propósito** (`ensure()` en `gateway.go`): sin eso, N
llamadas simultáneas levantarían N microVMs del mismo servicio y agotarían la RAM.
Medí ese guardián y lo llamé cuello de botella.

Con servicios **distintos** sí había paralelismo, aunque imperfecto. Y durante una
ráfaga de cinco: **96–97% de CPU en usuario, 0% ocioso, 0% de espera de E/S**. No
había nada que paralelizar — ya lo estaba, y saturaba los cuatro núcleos.

### Qué quemaba esa CPU

`runFrom` llama a `verifyIntegrity`, que hashea `overlay.ext4` en **cada**
instanciación. El overlay dorado son **512 MiB nominales**, y sha256 los lee
enteros: los huecos de un fichero disperso se leen como ceros y por el hash pasan
igual.

Medido en `fc-test`: **2.866 ms de los 4.280** que tardaba una instanciación. El
**67%**.

El comentario que justificaba la decisión razonaba bien pero partía de un supuesto
falso — *«el rootfs y el snap.file son pequeños»*—, equivocado por dos órdenes de
magnitud.

### El arreglo

Un dorado es **inmutable** desde que se congela: verificarlo en las 142
instanciaciones no aporta nada sobre verificarlo en la primera. Se recuerda el
veredicto, con huella de tamaño y fecha de los dos ficheros; si cambian, se vuelve a
verificar. La corrupción se sigue detectando, y se detecta igual de pronto.

El veredicto vive en memoria y no en disco a propósito: tras reiniciar el daemon se
verifica una vez más, que es barato y cubre una corrupción ocurrida mientras estaba
parado.

### Medido después

Despertar suelto: **4.350 ms** la primera vez (verifica) y **175–202 ms** las
siguientes. **23×.**

Carga concurrente por el gateway, mismo servicio:

| Concurrentes | Antes p50 | Ahora p50 | |
|---|---|---|---|
| 1 | 4,82 s | **0,70 s** | 6,9× |
| 5 | 11,16 s | **1,30 s** | 8,6× |
| 10 | 21,78 s | **2,13 s** | 10,2× |
| 20 | 44,08 s | **4,66 s** | 9,5× |

100% de éxito en los cuatro niveles, igual que antes.

Servicios distintos, en régimen permanente: 1 → 0,66 s · 2 → 0,90 s · 4 → 1,53 s ·
5 → 1,52 s. Cuatro y cinco cuestan lo mismo: ahí sí se ve el paralelismo que antes
tapaba el hasheo.

> **El coste de la primera vez sigue ahí**, y es correcto que siga: 4,4 s por
> snapshot y por arranque del daemon. Quien tenga 150 servicios los paga una vez
> cada uno, no 150 veces cada uno.

---

## 8. Los cuatro pendientes, cerrados

### `kling commit -warm=false`

El hijo caliente vive **dentro** del dorado y lo engorda: 39 MB → 120 MB en un
servicio de node. A 150 servicios son 12 GB. Sigue activo por defecto —el caso
común es tener pocos servicios y usarlos a menudo— pero ahora se puede cambiar
disco por latencia de despertar.

### El gateway anota la salud que ya ve

El `TODO(P1-4)` proponía un sondeo periódico. Hay algo mejor y gratis: **el gateway
ya sabe cuándo un servicio falla** —devuelve 502— y esa señal se tiraba. Nueve
servicios estuvieron 26 horas caídos mientras `status` decía «✓ 9».

Un sondeo activo levanta una microVM por servicio y por vuelta, y sólo mira cuando
le toca. Esto refleja lo que le pasa a quien usa el servicio, en el momento en que
le pasa. Se escribe **sólo al cambiar de estado**, para no meter una escritura a
disco por petición atendida, y un éxito posterior recupera el servicio.

### El listado usa el directorio, no el meta

`loadSnapshot` devolvía el nombre que declaraba `meta.json`. Dos directorios que
declararan el mismo nombre salían como duplicados indistinguibles —observado con
`sequentialthinking`: dos entradas, un solo directorio— y un meta incoherente
mostraba un servicio que `runFrom` **no puede instanciar**, porque resuelve por
directorio.

Era la causa que en §6 quedó sin identificar.

### El test que nunca pasó en ninguna parte

`TestLiveVMsEncuentraLaMaquinaPorSuSocket` lanzaba `sleep 30 --api-sock X`, y
`sleep` trata todos sus argumentos como duraciones: moría al instante con
`unrecognized option`. `cmd.Start()` tenía éxito igualmente —el fork funciona— así
que el test parecía correcto y buscaba un proceso que ya no existía.

Sólo corre en Linux y el desarrollo es en macOS, donde se salta. **Nadie lo vio
fallar nunca.**

> Mi primer arreglo tampoco valía: usé `sh -c 'exec sleep 30'`, y `exec` sustituye
> la imagen del proceso, así que el `cmdline` perdía la ruta del socket. Sin el
> `exec`, el shell sigue vivo y la conserva.

### Cómo se ejercitan los tests de Linux

No hay Go en las máquinas Linux. Se compila el binario de tests en cruzado, **y hay
que llevar el árbol**: varios tests leen `scripts/*.sh` con rutas relativas y sin él
fallan por un motivo que no tiene nada que ver con el código.

```sh
tar czf - --exclude=.git . | ssh lab 'tar xzf - -C /tmp/k'
GOOS=linux GOARCH=amd64 go test -c -o /tmp/t.test ./internal/machine
scp /tmp/t.test lab:/tmp/k/internal/machine/ && ssh lab 'cd /tmp/k/internal/machine && ./t.test'
```

**Los siete paquetes pasan en Linux**, no sólo en macOS.
