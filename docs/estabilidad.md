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

## 4. Lo que sigue abierto

### El arranque del invitado domina el despertar

26 ms de *thaw* frente a **7,8 s** hasta que el servidor escucha. El dorado se
congela con el `/reset` aplicado —que mata los procesos hijo— así que al restaurar
hay que volver a arrancarlos.

Es una tensión real de diseño, no un descuido:

- **sin `/reset`**: el dorado guarda un servidor caliente → rápido, pero rechaza el
  siguiente `initialize` con `400 Server already initialized`;
- **con `/reset`**: limpio y correcto, pero se paga el arranque del runtime.

Resolverlo pide capturar el servidor *caliente pero sin estado de sesión*, que hoy
el puente no sabe producir.

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

## 5. Evaluación: WebView en lugar de Chromium

### Lo medido

| Imagen | Lógico | En disco |
|---|---|---|
| **playwright** | **2,5 G** | **899 M** |
| semgrep | 786 M | 585 M |
| fetch | 89 M | 77 M |
| wikipedia | 67 M | 54 M |
| context7 / everything / memory / sequentialthinking | ~50 M | 36–39 M |

Playwright es **12–25× más grande** que cualquier MCP de node y por sí solo supone
el **45%** de los 2,0 GB totales. La motivación es real y está medida.

### La buena noticia: el puente ya es agnóstico

`cmd/kling-bridge/browser.go` no sabe que arranca Chromium. Lee
`/etc/kling/browser.json`:

```json
{ "sidecar": [...], "ready_url": "...", "session_args": [...] }
```

y su propio comentario lo dice: *«el puente no sabe (ni le importa) que es
Chromium: sólo arranca lo que diga `sidecar`, espera a `ready_url` y añade
`session_args`»*. Añadir un motor alternativo es **una receta de imagen y un
marcador**, no un rediseño. La arquitectura para «que sea una opción sin descartar
Chromium» ya existe.

### La mala: «WebView» no existe dentro de estas microVMs

WebView significa un motor embebido **que aporta el sistema operativo**: WKWebView
en macOS, WebView2 en Windows (que es Chromium), Android WebView (Chromium),
WebKitGTK en escritorio Linux. Las microVMs son **Alpine mínimo**: no hay ningún
WebView del sistema que embeber.

Y hay un obstáculo de protocolo: los servidores MCP de navegador hablan **CDP**
(Chrome DevTools Protocol). WebKitGTK no habla CDP, así que usarlo obligaría a
escribir un adaptador — mucho más trabajo que el ahorro.

### Los candidatos reales, en orden

1. **`chrome-headless-shell`.** Es el Chromium sin capa de interfaz, pensado
   exactamente para automatización. Habla el mismo CDP, así que **ningún cliente
   cambia**: es sustituir el `sidecar` del marcador. Es la primera medición que
   haría.
2. **El WebKit que trae Playwright** (`playwright install webkit`). Más ligero que
   su Chromium y soportado nativamente por Playwright, pero cambia el motor de
   render: hay sitios que se comportan distinto.
3. **WebKitGTK con un adaptador CDP.** Descartado: el coste de escribir y mantener
   el adaptador supera al ahorro.

### Lo que propongo

Medir (1) antes de comprometerse. La pregunta que decide es **cuánto baja de los
899 MB**, y no la sé: no la he medido, y estimarla sería inventar. El camino es
construir la receta con `chrome-headless-shell`, compararla contra la de playwright
en disco, en `mem.file` del dorado y en tiempo hasta servir, y **sólo entonces**
decidir el defecto.

Lo que sí está claro es que la infraestructura para tenerlo como opción, y para
cambiar el defecto si gana, **ya está construida**.
