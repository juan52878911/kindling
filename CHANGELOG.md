# Changelog

Todas las novedades relevantes de kindling. Los binarios pre-compilados están
en [Releases](https://github.com/juan52878911/kindling/releases) para
linux/amd64, linux/arm64, darwin/amd64 y darwin/arm64.

## v0.7.0 — 2026-09-23

**Sandboxes para agentes de código.** El núcleo gana lo que necesita un agente para
ejecutar lo que escribe sin tocar el host: exec en streaming, ficheros y sandboxes de
usar y tirar. Guía en [`docs/exec-sandbox.md`](docs/exec-sandbox.md).

| kindling | kindling-mcp |
|---|---|
| v0.7.x | v0.1.x |

### Novedades

- **`kling shell <ref>`**: una terminal interactiva dentro de la microVM, con
  pseudoterminal de verdad, redimensionado y Ctrl-C interrumpiendo lo de dentro y
  no la sesión. Es un cambio de protocolo (`Upgrade: kling-shell/1`) con tramas en
  los dos sentidos; el daemon valida cada una en vez de reenviar a ciegas. El
  agente monta `devpts` si falta, así que no hay que reconstruir imágenes.
- **`kling sandbox create|ls|renew|rm`** (`/sandboxes`): una microVM con exec, sin red
  por defecto, que se destruye al vencer su TTL (10 min por defecto). Desde un snapshot
  con exec arranca en ~300 ms con el estado de la plantilla.
- **`kling exec`** (`POST /machines/{ref}/exec`): stdout y stderr por separado y en
  streaming (NDJSON, también por SSH), stdin, entorno, directorio, plazo que mata al
  grupo de procesos (código 137) y topes de salida por flujo. Termina con el código del
  comando remoto. `?wait=1` da el resultado agregado.
- **`kling cp`** (`/machines/{ref}/files`): subir y bajar ficheros, con escritura
  atómica y sin seguir enlaces en el último componente.
- **`-on-ttl freeze` en los sandboxes**: en vez de destruirse al vencer, se
  duermen a coste cero y el siguiente exec los despierta en milisegundos. Con
  `freeze` el TTL cuenta inactividad; con `remove`, vida máxima.
- **`kling run -allow-exec` y `-on-ttl remove`.** `allow_exec` viaja por el API como
  opt-in explícito, se graba en el snapshot con `commit` y las instancias lo heredan;
  pedirlo sobre un snapshot sin él es `409`.
- El agente de invitado (`pkg/guest`, `kling-guest`) sirve `/exec/stream` y `/files`,
  solo con `kling.exec=1`.
- `GET /info` anuncia las capacidades `exec` y `sandboxes`.

### Correcciones de robustez

- **Un fallo posterior al arranque ya no deja un firecracker vivo.** Restaurar
  desde un snapshot y descongelar no mataban el proceso que acababan de lanzar
  —el arranque en frío sí lo hacía—, así que cada intento fallido (el caso real
  es el TSC invalidado tras reiniciar el host) retenía su RAM para siempre,
  invisible para `kling ps`. Descongelar, además, dejaba la red montada y la
  máquina figurando como congelada, y el siguiente intento readoptaba ese proceso
  vacío como sano.
- **Los VMM huérfanos se recogen en marcha**, no solo al arrancar el daemon.
- **El TTL no se reinicia al despertar**: se cuenta desde su propio reloj
  (`ttl_at`), no desde el último arranque. Un sandbox que dormía y despertaba
  podía no vencer nunca.
- **Una máquina con un secreto inyectado ya no reintenta congelarse cada 10 s
  para siempre**: se retira su TTL, una vez y diciéndolo en el log.
- **Antes de rechazar por memoria se pide prestado a los globos** de los
  invitados vivos, que devuelven lo que no usan sin congelar a nadie; y el
  desalojo puede soltar una instancia precalentada, que hasta ahora nunca ocurría.
- **El tope de máquinas del daemon se distingue de la falta de memoria** (409
  propio) y se puede subir con `KLING_MAX_MACHINES`.

### Cambios que se notan

- Las imágenes construidas antes de v0.7 llevan un agente sin exec en streaming: el
  daemon contesta `501` y pide reconstruirlas (`kling images toolchain`, o
  `kling images build -builder base`). `kling volume populate` sigue funcionando con
  ellas.
- `scripts/90-e2e.sh` prueba exec, ficheros y sandboxes, y compara con los mensajes en
  inglés del CLI.

## v0.6.0 — 2026-09-23

**El núcleo deja de llevar MCP.** Todo lo de alojar servidores MCP —el puente, el
gateway, el catálogo, `kling mcp`, `add`, `search`, `connect`, `export`, `memory`,
`migrate`— se muda a su propio repositorio y binario,
[kindling-mcp](https://github.com/juan52878911/kindling-mcp) v0.1.0. `kling` sigue
siendo el único comando: con kindling-mcp instalado, esos comandos aparecen en él
como antes, servidos por la extensión.

| kindling | kindling-mcp |
|---|---|
| v0.6.x | v0.1.x |

### Para actualizar

1. Actualiza kindling (`make deploy` o el instalador) y después instala
   kindling-mcp en tu máquina y en el host del daemon (su `make deploy`), que
   instala el puente, el empaquetador, el constructor `mcp` y las unidades
   `kling-gateway` y `kling-heal`, ahora con `ExecStart=/usr/local/bin/kling-mcp`.
2. Nada que migrar a mano: el daemon pasa catálogos, salud y links de v0.4 a
   anotaciones y store, y `kling` mueve la sección `memory` de la configuración a
   `extensions.mcp` (`kling config set mcp.memory.enabled true`).

### Novedades

- **Constructor `base` en el núcleo** (`scripts/81-base-image.sh`): una capa con
  paquetes de apk/apt y `kling-guest` como PID 1. `kling images toolchain` lo usa.
- **Unidades de extensiones en `kling up`**: el manifiesto declara `units` y
  `kling up` las arranca con el daemon si están instaladas.

### Cambios incompatibles

- Fuera del núcleo los comandos MCP y `kling-bridge`; `install.sh --bridge` avisa
  de que el puente viene con kindling-mcp.
- Retiradas las rutas deprecadas en v0.5 (`/snapshots/{n}/catalog`,
  `/snapshots/{n}/health`, `/links`, `/images/refresh-bridge`,
  `/images/{n}/capabilities`) y los campos `tools`/`health*` del snapshot. Ver
  [`docs/api.md`](docs/api.md#rutas-retiradas-en-v06).
- `POST /images` exige `builder`; el proxy al invitado exige `path` (salvo
  `probe_only`).
- `kling images refresh` pasa a ser `kling mcp refresh-bridge`.
- Fuera de `pkg/api` los tipos de MCP (`ToolSpec`, `Link`, `Capabilities`...);
  viven en kindling-mcp.

## v0.5.0 — 2026-09-23

kindling se separa en dos: **el núcleo de microVMs** y **kindling-mcp**, lo que se
venía usando para alojar servidores MCP. Esta versión hace la separación dentro del
repositorio sin que cambie nada de lo que se teclea: `kling mcp import`, `kling add`,
`kling connect` y el gateway funcionan igual, ahora servidos por una extensión
incorporada. El siguiente paso mueve kindling-mcp a su propio repositorio y binario.
Ver [`docs/extensions.md`](docs/extensions.md) y [`docs/api.md`](docs/api.md).

### Novedades

- **Extensiones de `kling`.** Un ejecutable `kling-<nombre>` en el `PATH` (o en
  `$KLING_PLUGIN_PATH`) añade subcomandos a `kling` declarándolos en un manifiesto:
  aparecen en la ayuda y en el completado, y `kling` les pasa el control con `exec`,
  así que códigos de salida y señales llegan intactos. Pueden añadir líneas a
  `kling status` y claves a `kling config`. `kling plugins` las lista. Los comandos
  MCP pasan por este mismo camino como extensión incorporada.
- **Anotaciones de snapshot y store en el daemon**, para que una extensión guarde
  su estado sin que el núcleo lo entienda. El catálogo y la salud de MCP son ahora
  las anotaciones `mcp.tools` y `mcp.health`; los servidores externos enlazados,
  `store/mcp/links`.
- **Constructores de imágenes con nombre.** `POST /images` con `builder` ejecuta un
  constructor de root instalado por el administrador en
  `/usr/local/lib/kindling/builders/`; `kling images build <nombre> -builder <b>`.
- **Ficheros dentro de imágenes**: `kling images cat` e `images put` leen o ponen al
  día un fichero de una imagen ya construida, sin reconstruirla.
- **`kling-guest`**, el agente de invitado genérico (exec, volúmenes, MMDS, DNS)
  para microVMs sin servidor MCP. `kling-bridge` lo embebe.
- **Paquetes públicos para extensiones**: `pkg/api`, `pkg/config`, `pkg/guest`,
  `pkg/scheduler` (planificación genérica del gateway), `pkg/plugin`,
  `pkg/transport`, `pkg/durable`, `pkg/panico`.
- **`GET /info` devuelve la versión real del daemon** (era siempre `0.1.0`) y la
  lista de capacidades del API.

### Cambios que se notan

- **`kling status -json`**: lo del gateway y los agentes pasa de `gateway` y
  `agents` a `extensions.mcp.gateway` y `extensions.mcp.agents`.
- **El proxy al invitado ya no asume MCP.** Quien pasa `path` recibe solo lo que
  pide. Las peticiones sin `path` (clientes v0.4) conservan los valores de antes
  hasta v0.6.
- Las rutas `/snapshots/{n}/catalog`, `/snapshots/{n}/health`, `/links`,
  `/images/refresh-bridge` y `/images/{n}/capabilities` quedan como alias
  deprecados; se retiran en v0.6. Al arrancar, el daemon migra `links.json` al
  store y deja el original como `links.json.migrated`.

### Correcciones

- **`-bundle` copia el `package.json` junto al bundle.** `server-sequential-thinking`
  lee su versión del `package.json` al arrancar, buscándolo junto al fichero que
  ejecuta; el bundle de esbuild vivía solo en `/opt` y el proceso moría con
  "Could not locate package.json for server version", que el gateway devolvía
  como 502 en el `initialize`. Ahora `scripts/80-mcp-image.sh` deja en
  `/opt/package.json` el del paquete que aporta el entry (el mismo que encontraría
  sin empaquetar). De los servidores oficiales de npm sólo éste lo hace; el SDK
  de TypeScript no.

## v0.4.0 — 2026-08-28

Ochenta y un commits desde v0.3.0. La versión va de **que funcione** a **que se
recupere solo y que no se caiga entera**: casi todo lo de abajo salió de mirar el
sistema vivo, no de leer código.

El fallo que mejor resume la tanda: `semgrep` y `playwright` estuvieron **297 horas
caídos** y nadie se enteró, porque la salud sólo se registraba cuando llegaba una
petición. Un servicio que nadie llama se queda roto en silencio hasta que alguien lo
llama.

### Novedades

- **`kling mcp heal`** con temporizador de systemd (`OnBootSec=2min`,
  `OnUnitActiveSec=6h`). Un reinicio del anfitrión invalida **todos** los dorados a
  la vez —Firecracker los ata a la frecuencia del TSC— y hasta ahora había que
  reimportarlos a mano. `heal` sondea, y sólo reconstruye lo que el TSC invalidó:
  un servicio enfermo por otra causa no se arregla rehaciéndolo, y reimportarlo
  sería ruido que tapa el problema real. Reconstruye con la configuración
  **original** —memoria, vCPUs, egress, volúmenes, etiquetas— no con la de por
  defecto.

- **`kling mcp verify` puede fallar.** Antes salía 0 sin ejercitar nada: pedía
  `tools/list` y se daba por satisfecho. Ahora llama a una herramienta de verdad
  (`browser_navigate` sobre `about:blank` en las imágenes de navegador) y consulta
  `/dns` del puente, que devuelve los nameservers del invitado y si resuelve.

- **`kling images rm`**, que se niega si la imagen es base de otra capa, la usa un
  dorado o tiene una máquina viva.

- **Base glibc con `chrome-headless-shell`** (`scripts/71-build-glibc-base.sh`).
  Medido contra el Chromium de Alpine sobre tres sitios reales: **misma cantidad de
  texto extraído**, 637 MB frente a 986,8 MB y 116 ms frente a 401 ms. Chromium se
  queda como opción; el puente no sabe qué motor arranca, lee
  `/etc/kling/browser.json`.

- **`images refresh` hace crecer la imagen** cuando el puente no cabe dentro, en vez
  de fallar. Y **graba la salud** en lugar de sólo imprimir un aviso: al refrescar
  invalida el dorado, y antes el servicio quedaba roto sin que nadie lo supiera.

### Correcciones — el 502 permanente

Tres fallos encadenados que se disfrazaban de uno solo, y que dejaban un servicio
devolviendo 502 para siempre:

- el recolector de basura medía **el sistema de ficheros entero**, así que se
  desataba por disco que no era suyo;
- el gateway **cacheaba la instancia muerta** y seguía marcándola hacia ella;
- la salud se anotaba **al adquirir** la instancia, no según el resultado, así que
  un servicio roto se reafirmaba sano en cada intento fallido.

Medido después: `memory` y `sequentialthinking` pasan de 502 a 200 en **688 ms**.

### Correcciones — seguridad

- **El gateway ya no reenvía su propio token.** Lo mandaba al invitado y a URLs de
  terceros: un servidor MCP comprometido se llevaba la credencial del agregador.
- **Las imágenes dejan de ser world-readable.** Contenían los ficheros `-env` con
  los secretos de cada servicio; ahora se hace `chown` al usuario del servicio con
  `0640`/`0750`.
- **`cpu` no es `cpuset`.** La detección de controladores de cgroup usaba
  `Contains`, y `cpuset` contiene `cpu` como subcadena: el límite se daba por puesto
  sin estarlo. Ahora se compara palabra a palabra.
- **Tests del cortafuegos de salida**: `isBlockedIP` cubre RFC1918, loopback,
  link-local —incluido el `169.254.169.254` de metadatos—, CGNAT, multicast y sus
  equivalentes IPv6; y `ParseEgress` **falla** ante un valor desconocido en vez de
  caer en el más permisivo.

### Correcciones — robustez

- **Los pánicos de los bucles de fondo quedan contenidos.** Había 21 goroutines y
  **cero** `recover()`: un nil-pointer en el reconciliador o en el persistidor de
  estado mataba el proceso y dejaba huérfanas todas las microVM. Se envuelve **cada
  iteración**, no el bucle: contener el bucle entero dejaría el daemon vivo sin
  reconciliar nada, que es peor porque no se nota.
- **Registro de cerrojos con contador de referencias.** El `sync.Map` de antes
  borraba la entrada mientras otra goroutine seguía esperándola. Medido rompiendo el
  código a propósito: **1.758 entradas dobles** en la sección crítica.
- **Volúmenes: comprobar y reservar bajo el mismo cerrojo.** Dos arranques
  simultáneos podían quedarse el mismo volumen exclusivo. `RemoveVolume` tenía la
  misma carrera entre la comprobación y el `os.Remove`.
- **Escritura durable en un solo sitio**: fichero temporal, `fsync` del fichero,
  `rename`, `fsync` del directorio. Antes sólo `state.json` hacía `fsync`;
  `links.json`, la configuración, `meta.json` y las recetas de imagen no.
- **Matar el grupo de procesos, no sólo el pid del hijo**, para que no queden nietos
  huérfanos.
- **Los topes de tamaño fallan en vez de truncar.** Un JSON cortado por la mitad no
  es un JSON pequeño: es ilegible, y el error decía otra cosa.
- **`kling commit` exige que el invitado SIRVA** antes de congelar un dorado. Un
  snapshot tomado antes de tiempo restaura en 26 ms y luego no contesta, minutos u
  horas después, con un error que no menciona el commit.
- **`evictLRU` reponía la víctima** que no se pudo congelar, y prueba con otra en vez
  de rendirse.
- **`MCPPayload` elegía el primer evento SSE**, que puede ser una notificación; ahora
  busca la respuesta.
- **`callLink` reintentaba ante cualquier error**, incluidos los tiempos de espera;
  ahora sólo ante sesión caducada.
- Dos deref nil en `Freeze`/`Thaw`, `Stop` sin el cerrojo de ciclo de vida, `Remove`
  borrando su entrada demasiado pronto, y un firecracker huérfano al hacer `Thaw`.

### Rendimiento

- **El despertar baja de 4.350 ms a 175–202 ms**, y una carga de 20 peticiones de
  44,08 s a 4,66 s. El puente deja un hijo **caliente sin ligar**, así que el dorado
  no paga el arranque del runtime al restaurar.
- **El veredicto de integridad del snapshot se recuerda** en vez de rehashear 512 MiB
  en cada uso.

### Tests

Los cinco paquetes que no tenían ninguno: `internal/report`, `internal/assets`,
`internal/config`, `internal/events` y `cmd/notas-server`. Más los del registro de
cerrojos, la retirada de imágenes, el transporte, el cliente de Firecracker y el
cortafuegos. Cada arreglo se verificó **rompiendo el código a propósito** y
comprobando que el test se pone rojo.

### Actualizar desde v0.3.0

Un cambio de comportamiento, de la auditoría de entorno de más abajo: el puente
escucha en `127.0.0.1:9100` en vez de `0.0.0.0:9100`. Si el gateway corre en otra
máquina, hace falta `-listen 0.0.0.0:9100` explícito.

Instalar el temporizador de autocuración:

```sh
sudo cp packaging/kling-heal.service packaging/kling-heal.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kling-heal.timer
```

### Correcciones — auditoría de supuestos del entorno

Siete fallos que **no daban ningún error**: cada uno reportaba éxito mientras no
hacía lo que decía. Todos verificados en ejecución, no sólo leídos.

- **`kling memory install-service` instalaba un LaunchAgent que no podía arrancar,
  y salía 0.** `bridgePath()` podía devolver la ruta *relativa* `./kling-bridge-local`,
  y el cwd de launchd es `/`: el job moría con `EX_CONFIG` (78), con stderr vacío y
  sin escribir el log. Ahora la ruta es siempre absoluta, y se consulta a launchd si
  el job vive de verdad — `launchctl load` devuelve 0 aunque nunca llegue a arrancar.
  Los dos códigos que no dicen nada por sí solos se traducen: **78** (ruta relativa o
  inexistente) y **126** (sin permiso, típico bajo `~/Documents`, `~/Desktop` o en un
  volumen externo por TCC).

- **El puente ya no se expone a la red por defecto.** `-listen` pasa de
  `0.0.0.0:9100` a `127.0.0.1:9100` en `memory enable` e `install-service`. Lo que se
  envuelve suele ser la memoria personal, el puente **no autentica** —ni puede
  hacerlo de forma útil, porque `kling mcp link` no manda cabeceras— y `/reset` queda
  accesible para cualquiera que alcance el puerto. Exponerlo sigue siendo posible y
  legítimo con el gateway en otra máquina, pero ahora es una decisión y avisa.
  *Cambia comportamiento:* si el gateway corre en otro host, hace falta
  `-listen 0.0.0.0:9100` explícito.

- **El daemon arrancaba «sano» en un host donde no podía hacer nada.** Sólo se
  comprobaban `ip` e `iptables`. Ahora también `firecracker`, `setpriv` y `mkfs.ext4`,
  y los nombra todos de golpe. `setpriv` era el peor: no lo cubría ninguna
  comprobación y fallaba por microVM en pleno arranque, sin relación visible con la
  causa.

- **`install.sh` podía aceptar un checksum sin verificar nada.** Con `sha256sum`
  ausente, `EXPECTED` y `ACTUAL` quedan vacíos y `"" != ""` es falso: la comprobación
  **pasa**. Añadidas las guardas de vacío en los dos bloques.

- **El puente se aceptaba por existir, no por ser ejecutable en el destino.**
  `[ -f ]` no dice nada del formato: un Mach-O de `make bridge-local` pasaba, se
  instalaba como PID 1 de una imagen x86-64, y la microVM reventaba con un pánico del
  kernel sin mencionar al puente. Ahora se comprueba el mágico `\x7fELF`.

- **`printf %q` es de bash y los entrypoints son `#!/bin/sh`.** En Alpine eso es
  busybox ash, y para un argumento con tabulador `%q` emite `$'x\ty'`, sintaxis que
  dash no entiende. Sustituido por entrecomillado POSIX con comilla simple.

- **Una ruta de socket demasiado larga daba `bind: invalid argument`**, sin mencionar
  ni la longitud ni el socket. El límite de `sun_path` son 104 bytes en macOS y 108 en
  Linux; ahora se dice.

## v0.3.0 — 2026-08-13

Notas completas, con tablas comparativas: [`docs/RELEASE-v0.3.0.md`](docs/RELEASE-v0.3.0.md).

La v0.2.0 hizo kindling instalable, autenticado y con estado. Esta lo hace denso, paralelo
y compartido, y estrena soporte (limitado) para Mac Apple Silicon.

### Novedades

- **Misma herramienta en paralelo.** El gateway crea **réplicas por servicio** bajo demanda
  desde el snapshot dorado (COW); varias sesiones concurrentes ya no las topa el cap de
  sesión del puente.
- **`kling migrate`.** Mueve un MCP a kindling **conservando el nombre de la entrada y de
  las herramientas** (endpoint per-servicio): las skills que lo usaban siguen funcionando
  sin reescribirse.
- **Secretos por sesión vía MMDS**, inyectados en la microVM viva; un snapshot congelado
  nunca lleva secretos dentro.
- **Egress allowlist de dominios** (tercer modo, fail-closed): solo salen los dominios
  declarados, con resolver dinámico DNS→ipset.
- **Cuotas por token/tenant** en el gateway (reparto justo).
- **Devolver la RAM**: `kling squeeze` (balloon) reclama la memoria disponible; `/metrics`
  y `kling top` (PSS) hacen visible el peso real, contando el `mem.file` compartido.
- **Modo proxy HTTP/SSE** en el puente: soporta MCP que no hablan stdio.
- **Auto-detección de capacidades** (navegador/internet/nativo) y Chromium compartido con
  contexto por sesión.
- **zram opt-in** en el host para densificar.
- **Mac Apple Silicon (arm64), compatibilidad limitada.** `make deploy-mac` y binarios
  `kling-darwin-arm64`/`kling-linux-arm64`. Requiere M3+ y virtualización anidada; el
  arranque en frío es ~16 s bajo KVM anidado (vs ~3 s en Linux nativo) y el paralelismo
  práctico ronda ~8 réplicas. Límites y receta en [`docs/mac-arm64.md`](docs/mac-arm64.md).

### Correcciones

- `mcp import` respeta `-cpus` y `defaults.mem_mib`.
- El puente **recicla la sesión más ociosa** al llegar al tope (reconexión limpia en
  servicios de 1 sesión); `-e KEY=VAL` para hornear env que apagan el phone-home (semgrep:
  124 s → ~10 s).
- GET sin sesión a un servicio devuelve **405, no 404** (clientes streamable-HTTP cargan
  el endpoint per-servicio).
- Segador/evict sin perder trabajo en vuelo, reconciliación de rutas pegajosas por vida,
  suelo de `MemFree` y cierre del TOCTOU de memoria. Jailer opt-in en frío/restauración,
  matando VMMs huérfanos. Integridad (sha256) y salud del catálogo.

### Actualizar desde v0.2.0

- Corre `kling images refresh`: el puente trae el modo proxy y la inyección de secretos.
- El paralelismo de la misma herramienta no pide configuración; ajusta las cuotas por
  tenant si repartes un mismo token.
- En Mac: necesitas M3+ y `nested virt` (ver [`docs/mac-arm64.md`](docs/mac-arm64.md)).

## v0.2.0 — 2026-08-11

Notas completas, con tablas comparativas: [`docs/RELEASE-v0.2.0.md`](docs/RELEASE-v0.2.0.md).

La v0.1.0 demostraba que la idea funciona: microVMs que descongelan en milisegundos y un
gateway que las despierta bajo demanda. Esta la hace instalable, autenticada y capaz de
guardar estado.

### Novedades

- **`kling up` y `kling status`.** Instalar deja de ser tres scripts a mano como root: el
  kernel y la imagen base van dentro del binario.
- **El gateway exige token.** Despertar un snapshot es ejecutar código, y el gateway es lo
  único que escucha en red. Se genera solo la primera vez.
- **7 clientes de IA** en `connect -install`, frente a 2.
- **Catálogo oficial**: `kling search` y `kling add` contra `registry.modelcontextprotocol.io`.
- **Volúmenes persistentes**, con journal, hasta cuatro por microVM y compartibles en solo
  lectura: un escritor exclusivo, o cuantos lectores hagan falta.
- **`kling volume populate`**: instala paquetes DENTRO de una microVM desechable en vez de
  como root en el anfitrión.
- **`kling images toolchain`**, **`kling images refresh`** y **`kling images recipe`**.
- **NODE_PATH y PYTHONPATH automáticos** apuntando a los volúmenes que traen paquetes.

### Correcciones

Nueve fallos que solo aparecen metiendo servicios de verdad, entre ellos: `mcp import`
ignoraba `defaults.mem_mib` (la causa real de los timeouts en paralelo que se achacaban al
gateway), los snapshots no guardaban ni su política de red ni su volumen, los volúmenes se
formateaban sin journal, y el segador del gateway congelaba microVMs con trabajo en vuelo.

### Actualizar desde v0.1.0

- El gateway pide token: cópialo con `kling config set gateway.token …`.
- Reimporta los servicios: sus snapshots no guardan la política de red.
- Corre `kling images refresh`: el puente vive dentro de cada imagen.

## v0.1.0 — 2026-08-08

Primera release con binarios distribuidos. Antes de esta versión, `kling` solo
estaba disponible vía `make install` (compilación local).

### Novedades

- **Instalación con una línea** desde releases: `curl -fsSL .../install.sh | sh`.
  Sin Go instalado, sin clonar el repo, sin sudo.
- **Releases multi-plataforma** vía GitHub Actions: binarios pre-compilados para
  Linux (amd64/arm64), macOS (amd64/arm64). El bridge se publica por separado
  porque va dentro de las microVMs (estático, musl-safe).
- **Verificación SHA256** antes de instalar: cada release incluye `SHA256SUMS`
  y el instalador aborta si el checksum no coincide.
- **CLI `--dry-run`** para previsualizar qué se descargará y dónde quedará.

### Arreglos desde la última versión funcional (HEAD)

- **Snapshots stateful ya no rompen los restores.** El daemon ahora expone
  `POST /reset` en el bridge, y `kling mcp import` lo invoca (o espera al
  auto-reset del wrapper HTTP) antes de hacer commit. Sin esto, los snapshots
  dorados se congelaban con el servidor ya inicializado, y al restaurar el
  puerto 8080 nunca abría o el handshake daba 400/406.
- **`everything` (HTTP nativo)** ya funciona end-to-end con el wrapper de
  auto-reset (`/var/run/kling-http-reset-done` persiste en el overlay).
- **`filesystem-mcp` (stdio + bridge)** ya funciona end-to-end: el bridge
  tiene el endpoint `/reset` y `mcpImport` lo invoca tras capturar el catálogo.
- **CLI actualizado detecta las nuevas respuestas del daemon** — antes, un CLI
  viejo podía mostrar mensajes de error engañosos.

### Limitaciones conocidas

- **Windows no soportado.** `internal/machine/manager.go` usa `syscall.Kill`,
  `Setsid`, `Stat_t` que son POSIX. Si necesitas Windows, abre un issue.
- **Solo se publica el CLI para macOS** — el daemon requiere KVM, que en macOS
  no existe fuera de máquinas virtuales con VT-x anidado (que es justamente
  cómo se mide aquí: Proxmox + KVM + Firecracker).
- **El bridge solo se publica para Linux.** Va dentro de microVMs Alpine (musl);
  en macOS no tiene sentido empaquetarlo.