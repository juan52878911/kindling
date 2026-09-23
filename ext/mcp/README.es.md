# kindling-mcp

Herramientas MCP serverless sobre las microVMs Firecracker de
[kindling](https://github.com/juan52878911/kindling). Coge cualquier servidor MCP de código
abierto y conviértelo automáticamente en un servicio que se levanta bajo demanda, en
milisegundos, con aislamiento a nivel de kernel.

kindling-mcp es una **extensión de `kling`**: no añade un comando nuevo que aprender. Una
vez instalada, `kling mcp`, `kling add`, `kling search`, `kling connect`, `kling gateway`,
`kling export`, `kling memory` y `kling migrate` aparecen en el mismo `kling` que ya usas,
en su ayuda y en su completado.

> Estado: **v0.1.0** — la mitad MCP de kindling v0.4/v0.5, ahora por su cuenta encima del
> núcleo kindling v0.6. Para quien ya lo usaba no cambia nada: los mismos comandos, los
> mismos snapshots (el daemon los migra en su sitio), el mismo gateway.

**El invitado se asume hostil**: no se sabe qué servidor MCP acabará alojado. Mira el
[SECURITY.md](https://github.com/juan52878911/kindling/blob/main/SECURITY.md) de kindling.

## Instalación

Primero kindling — el núcleo con el daemon — y esta extensión encima:

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling-mcp/main/scripts/install.sh | sh
kling plugins                 # debería salir kling-mcp
```

Eso instala `kling-mcp` y `kling-bridge` en tu máquina. El host del daemon necesita su
mitad — el gateway, la autocuración, el empaquetador y el puente que va dentro de cada
imagen:

```sh
make deploy HOST=ssh://juan@192.168.2.60   # después del make deploy de kindling
make deploy-mac HOST=ssh://usuario@vm-lima # VM Linux arm64 en Apple Silicon
```

| kindling | kindling-mcp |
|---|---|
| v0.6.x | v0.1.x |
| v0.7.x | v0.1.x, v0.2.x |
| v0.8.x | v0.3.x |

El instalador se niega a instalar junto a un `kling` más viejo que el mínimo que declara
la extensión.

## Si vienes de kindling v0.5 o anterior

Antes de v0.6 todo esto iba dentro de `kling`. Tras actualizar kindling, instala esta
extensión y todo sigue funcionando: el daemon migra catálogos, salud y links a sus
anotaciones y su almacén genéricos, y la configuración `memory.*` antigua pasa sola a
`mcp.memory.*`. Vuelve a desplegar las unidades de systemd con `make deploy` para que
llamen a `kling-mcp` en vez de a `kling`.

---

# Servicios MCP

## Catálogo: buscar y añadir servidores MCP

No hace falta saber empaquetar nada. `kling` habla con el registro oficial
(`registry.modelcontextprotocol.io`):

```sh
kling search filesystem                    # qué hay, y qué puede empaquetar solo
kling add io.github.domdomegg/filesystem-mcp
```

`kling add` construye la imagen, arranca una plantilla, le pregunta qué sabe hacer, la
congela como snapshot dorado y guarda su catálogo. Desde entonces, listar sus capacidades
**no despierta la microVM**.

Empaqueta servidores de **npm y de PyPI** que hablen stdio (npm gana si el servidor
publica en los dos). Cuando `search` dice que un servidor no se puede empaquetar sin
atención, explica por qué y cuál es la alternativa, en vez de fallar a mitad de
construcción. Flags útiles:

| Flag | Qué hace |
|---|---|
| `-bundle` | colapsa `node_modules` en **un** fichero con esbuild — medido: 1205 ficheros → 1, `initialize` en frío de ~7 s → ~2,5 s. La palanca mayor en Mac/arm64 |
| `-base node` / `-base python` | construye una **capa** pequeña sobre una base de runtime compartida en vez de una imagen monolítica ([imágenes por capas](https://github.com/juan52878911/kindling/blob/main/README.es.md#imágenes-por-capas-una-base-por-familia-de-runtime)); se elige sola si existe una base con el nombre de la familia |
| `-env KEY=value` | hornea interruptores de entorno en el entrypoint (texto plano: para toggles, **no para secretos** — esos van [por MMDS](https://github.com/juan52878911/kindling/blob/main/README.es.md#secretos-que-nunca-tocan-un-snapshot-mmds)) |
| `-cmd "..."` | sustituye el comando de arranque inferido (los entry points de PyPI se infieren por convención y se verifican al construir) |
| `-volume nombre[:/punto][:ro]` | engancha [almacenamiento persistente](https://github.com/juan52878911/kindling/blob/main/README.es.md#volúmenes-lo-que-sobrevive-a-la-microvm), repetible |
| `-dry-run` | enseña qué haría sin hacerlo |

kindling además **auto-detecta capacidades**: si un servidor necesita navegador, salida a
internet o binarios nativos, la imagen y la política de red de la máquina se configuran
en consecuencia — un servidor de navegador recibe un Chromium compartido con un contexto
por sesión.

## Convertir cualquier servidor MCP en un servicio

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data

kling mcp import filesystem
```

`mcp import` hace el ciclo entero:

```
Importing "filesystem" from image "filesystem"

  1/5  starting the template... ✓ 0aeee853 at 172.30.0.6
  2/5  waiting for the MCP server... ✓
  3/5  asking what it can do... ✓ 14 tool(s)
  4/5  freezing as golden snapshot... ✓
  5/5  saving the catalog... ✓
```

El paso 3 es **introspección**: al servidor se le pregunta qué sabe hacer exactamente una
vez, y el paso 5 guarda ese catálogo junto al snapshot. La memoria de la plantilla, sus
vCPUs, la política de egress, los volúmenes y las etiquetas quedan **horneados en el
snapshot** — y se reutilizan tal cual cuando el servicio se
[cura](#autocuración-kling-mcp-heal) o se refresca.

**`-n` preinstala los paquetes npm en la imagen.** No es comodidad: las microVMs
arrancan sin salida a internet, así que un `npx -y` en runtime fallaría al descargar.

### Listar el inventario no toca los servicios

Sin catálogo persistido, preguntar "¿qué herramientas hay?" fuerza un `tools/list` contra
cada servidor, y eso **despierta sus microVMs**. Una pregunta de inventario acabaría
arrancando veinte máquinas.

Con `mcp import`, el catálogo vive en disco junto al snapshot:

```sh
kling mcp list -v          # cada herramienta, sin arrancar nada
kling mcp refresh <svc>    # recaptura tras actualizar el servidor
```

Verificado: listar el inventario completo a través del agregador deja el contador de
máquinas en **0**.

### Tres cosas que hay que saber al empaquetar un servidor de node

- **`-n` preinstala el paquete npm.** Las microVMs arrancan sin salida a internet: un
  `npx -y` fallaría al descargar.
- **El `entrypoint` es PID 1 y el kernel no le da PATH.** Sin fijarlo, los binarios que
  instala npm no se encuentran: `executable file not found in $PATH`.
- **Los directorios que el servidor espera deben existir DENTRO de la imagen.**
  `server-filesystem` quiere `/data`; crearlo en el host no sirve de nada.
- **Si el servidor habla HTTP, tiene que escuchar donde se le dice.** El entrypoint fija
  `PORT=8080` y el servidor debe servir el protocolo en `/mcp`: ahí mira el gateway.

## Cualquier servidor MCP, alojado bajo demanda

La mayoría de los servidores MCP de código abierto solo hablan **stdio**: un proceso hijo
persistente al que hablas por tuberías. No hay puerto al que llamar, y el cliente dicta
el ciclo de vida. Es lo contrario de invocable bajo demanda.

`kling-bridge` corre **dentro** de la microVM, lanza el servidor como hijo y expone su
protocolo por Streamable HTTP:

```
gateway ──HTTP──> kling-bridge ──stdin/stdout──> servidor MCP
```

Desde fuera, un servidor stdio parece HTTP nativo. Envolver uno es una línea:

```sh
make bridge
sudo ./scripts/80-mcp-image.sh stdio files -p "nodejs npm" -- \
     npx -y @modelcontextprotocol/server-filesystem /data

kling run -name files-tmpl -image files -service files
kling commit files-tmpl files && kling stop files-tmpl
```

### Servidores que ya hablan HTTP

Si el servidor habla **Streamable HTTP nativo** no hace falta puente: escucha él mismo y
el gateway le habla directo. El modo `http` acepta las mismas opciones:

```sh
sudo ./scripts/80-mcp-image.sh http everything -p "nodejs npm" \
     -n "@modelcontextprotocol/server-everything" -- mcp-server-everything streamableHttp

kling mcp import everything -image everything
```

Dos condiciones, y el entrypoint generado fija ambas:

- **escuchar en `$PORT` (8080)**, que es donde mira el gateway dentro del invitado
- **servir el protocolo en `/mcp`**, que es la ruta que llama

Probado con `@modelcontextprotocol/server-everything`, el servidor de referencia del
protocolo. La imagen no lleva `kling-bridge` por ningún sitio:

```
$ kling connect everything
Status:    ✓ mcp-servers/everything v2.0.0 · 12 tool(s): echo, get-sum, …

$ call_tool everything.get-sum {"a":100,"b":23}
The sum of 100 and 23 is 123.
```

Hay un tercer modo: el puente también puede actuar como **proxy HTTP/SSE** para
servidores que hablan HTTP pero conviene que arranquen bajo su supervisión — útil en
servicios sin estado, donde un único proceso caliente se comparte y se congela vivo
dentro del snapshot (medido en `context7`: primer `initialize` 25,7 s como stdio → 2,4 s
estable como http-proxy).

### El circuito completo

```
modelo local  ──>  gateway  ──>  microVM  ──>  servidor MCP
  (tu Mac)        (Proxmox)     (Firecracker)   (stdio o HTTP)
```

[examples/agent/agent.py](examples/agent/agent.py) lo cierra: un cliente MCP más un bucle
de tool-calling contra ollama.

```
$ python3 examples/agent/agent.py "usa echo para decir hola"
→ kindling-echo v1.0.0  sesión b2787e00
→ herramientas: echo, session_info
→ llamando echo({"text": "hola"})
← hola
```

El modelo no sabe nada de microVMs: pide una herramienta y la herramienta aparece. Si
llevaba un rato sin usarse estaba congelada, y despertarla cuesta milisegundos.

| Camino | Latencia |
|---|---|
| Handshake MCP en frío, desde el Mac | **310 ms** |
| Llamada a herramienta, en caliente | **9 ms** |

## Sesiones y réplicas en paralelo

MCP identifica las conversaciones con `Mcp-Session-Id`, y un servidor stdio es **de
sesión única por naturaleza**: su estado vive en el proceso. De ahí:

- **El puente lanza un proceso hijo por sesión.** Dos conversaciones concurrentes no se
  pisan el estado.
- **El gateway enruta con pegajosidad.** La misma sesión vuelve siempre a la misma
  microVM; mandarla a otra instancia encontraría un servidor sin ese estado.
- **La misma herramienta puede usarse en paralelo.** Cuando las sesiones concurrentes
  superan lo que sirve una instancia, el gateway crea **réplicas por servicio** bajo
  demanda desde el snapshot dorado (copy-on-write, así que comparten memoria).
  Verificado con 4 sesiones concurrentes contra un servicio cuyo puente topa en 1 sesión.
- **Cuando un puente llega a su tope de sesiones**, recicla la más ociosa en vez de
  negarse, y los clientes reconectan limpio.

Demostrado con la herramienta `session_info`, que informa de su pid y su contador:

```
sesión 1 (3 llamadas extra):  pid=305 llamadas_en_esta_sesion=5
sesión 2 (recién creada):     pid=309 llamadas_en_esta_sesion=1
```

## Modo efímero: una microVM por acción

```sh
kling gateway -ephemeral -prewarm 3
```

Cada llamada recibe **su propia microVM**: se toma una del fondo de máquinas
pre-calentadas, sirve la acción y se destruye. Nace, actúa y muere.

```
acción 1: 19 ms   pid=305 llamadas_en_esta_sesion=1
acción 2: 24 ms   pid=305 llamadas_en_esta_sesion=1
acción 3: 19 ms   pid=305 llamadas_en_esta_sesion=1
```

`llamadas_en_esta_sesion=1` **en todas**: ninguna acción ve lo que hizo la anterior.

### De 350 ms a 19 ms

Perfilando una acción efímera sin optimizar:

| Etapa | Coste |
|---|---|
| Restaurar la microVM | 131 ms |
| Esperar a que vuelva la red | 53 ms |
| `initialize` (lanza el servidor MCP) | 61 ms — **con node son 300-500 ms** |
| `tools/call` | 9 ms |
| Destruir la máquina | ~100 ms |

Todo salvo `tools/call` puede pagarse antes o después:

- **`-prewarm N`** mantiene N instancias ya restauradas y **con su sesión MCP abierta**.
  La llamada se salta restaurar, esperar la red e inicializar.
- **La destrucción es asíncrona.** Antes iba en un `defer`, así que el cliente esperaba
  el desmontaje del namespace y el borrado de ficheros: 100 ms sobre una llamada de 2 ms.
  La máquina muere igual; el cliente simplemente ya no espera a que ocurra.

Resultado: **2 ms de ejecución real, 19 ms de punta a punta.**

La contrapartida es que no hay estado entre llamadas. Las herramientas que lo necesitan —
memoria, razonamiento paso a paso — tienen que usar la ruta con sesión
(`/mcp/<servicio>`), que mantiene el proceso vivo.

## Efímero o persistente: se decide solo

Una microVM efímera muere con todo lo suyo, memoria **y disco**. Así que la pregunta no
es "¿el servidor guarda estado?" sino:

> ¿algo que escribe una llamada tiene que verlo una llamada posterior?

`kling mcp import` lo infiere del catálogo y lo dice:

```
eco          EPHEMERAL    because it only queries: nothing to preserve
notas        PERSISTENT   because it writes with guardar_nota and reads with session_info
filesystem   PERSISTENT   because it writes with write_file and reads with read_file
memory       PERSISTENT   because read_graph suggests it accumulates context
thinking     PERSISTENT   because sequentialthinking suggests it accumulates context
```

**`filesystem` también es persistente**, aunque no lo parezca: escribe en el disco del
invitado, que es exactamente igual de volátil que su memoria.

La señal fiable es estructural — que el servidor exponga a la vez herramientas de
escritura y de lectura — con un puñado de palabras contrastadas contra el NOMBRE de la
herramienta para servidores que solo tienen una. Buscar esas palabras en las
descripciones clasificaba todo como persistente: "session" o "sequence" aparecen de
pasada en casi cualquier texto.

Se puede forzar con `-stateful` o `-ephemeral`.

### Ante la duda, persistente

Equivocarse hacia efímero produce **pérdida silenciosa**: la llamada responde bien y lo
escrito desaparece. Equivocarse hacia persistente solo cuesta una instancia congelada,
que no gasta ni CPU ni RAM.

El agregador lo marca en su inventario para que el modelo lo sepa:

```
memory [remembers between calls]: create_entities, add_observations, ...
filesystem: read_text_file, write_file, list_directory, ...
```

### Persistente no significa siempre encendido

Un servicio persistente conserva su estado, pero **deja de consumir cuando termina**.
Medido con `-idle 30s`:

```
write /data/note.txt               →  Successfully wrote
read /data/note.txt (otra llamada) →  "this must survive"

en marcha:    running   41 MiB sobre la línea base
a los 35 s:   warm      la RAM vuelve a 0   (freeze 661 ms)
al volver:    "this must survive"           (thaw 18 ms)
```

Congelar no es apagar: la instancia deja de existir como proceso — cero CPU, cero RAM —
pero su estado queda en disco y vuelve en milisegundos. El coste es el fichero de
memoria: 151 MiB de este servicio mientras está congelado.

## Una sola entrada para todos los servicios

Un cliente MCP carga las definiciones de **todas** las herramientas al conectar. Con
veinte servicios de diez herramientas son doscientos esquemas JSON en el contexto del
modelo antes de empezar a trabajar.

```sh
kling connect -all -install opencode                    # todos los servicios
kling connect -all -only eco,notas -install opencode    # solo algunos
kling connect -all -expand                              # catálogo completo
```

El endpoint `/mcp/_all` es un servidor MCP que enruta a los demás. Tiene dos modos:

**`proxy`** (por defecto) — expone **cuatro meta-herramientas** en vez de N:

| | |
|---|---|
| `list_services` | qué servidores hay y cuántas herramientas tiene cada uno |
| `find_tools` | busca por palabra clave; devuelve nombres y descripciones, **sin esquemas** |
| `describe_tool` | el esquema completo de una sola herramienta |
| `call_tool` | ejecuta, enrutando a la microVM que haga falta |

El modelo busca lo que necesita, pide el esquema de lo que va a usar, y llama.

**`expand`** — aplana el catálogo con nombres `servicio.herramienta`, para clientes que
funcionan mejor con todo cargado.

### Cuál sale más barato depende de cuántas herramientas tengas

El modo `proxy` tiene un **coste fijo** de ~300 tokens; `expand` crece con cada
herramienta. Con pocas, proxy sale **más caro**. Por eso `connect -all` lo mide contra tu
catálogo real y te lo dice:

```
Context cost, with your current catalog:
  proxy    3 definitions   ≈  248 tokens
  expand  28 definitions   ≈ 4327 tokens
  → The proxy mode you're using saves 4079 tokens.
```

El cruce está sobre las 8 herramientas. Con 28, proxy ahorra **17x**; con 200, decenas de
miles de tokens en cada conversación.

### El inventario va en el handshake

Los nombres de herramienta son baratos — 27 cuestan ~100 tokens — lo caro son los
esquemas de argumentos. Por eso `initialize` devuelve el **inventario completo** en su
campo `instructions`:

```
Available tools, grouped by service:

filesystem: read_text_file, write_file, list_directory, ...
memory [remembers between calls]: create_entities, ...

Call them with call_tool and the full service.tool name
(e.g. filesystem.read_text_file). If you don't know its arguments,
ask describe_tool first. find_tools is for searching by keyword.
```

Así el modelo sabe qué hay **desde el primer momento** y va directo a `call_tool`, en vez
de gastar una llamada en descubrir. Quedan tres meta-herramientas: `find_tools`,
`describe_tool` y `call_tool`.

### Búsqueda bilingüe

El modelo pregunta en el idioma del usuario y las herramientas están descritas en inglés.
Buscar "leer un fichero de texto" contra *"Read the complete contents of a file"* no
casaba ni un término, así que `find_tools` devolvía basura. Una tabla de sinónimos de
dominio — leer/read, fichero/file, carpeta/directory… — lo arregla sin meter un motor de
búsqueda.

## Reparación de tipos

Varios clientes MCP y modelos estropean los tipos JSON antes de enviarlos: los arrays
llegan como objetos con claves `"0"`, `"1"`, los números como cadenas, los booleanos como
`"true"`. El servidor los rechaza con "expected array, received object", y desde fuera
parece un fallo de la herramienta cuando la herramienta ni siquiera vio la llamada.

Como el catálogo guarda el esquema declarado de cada herramienta, el agregador deshace el
destrozo antes de reenviar. Se reconocen cuatro formas rotas donde el esquema pide un
array:

| Qué llega | Reparado a |
|---|---|
| `{"0":…,"1":…}` objeto indexado | `[…,…]` |
| `"[{…}]"` cadena con JSON dentro | `[{…}]` |
| `{…}` un objeto pelado | `[{…}]` |
| `{"paths":{"paths":[…]}}` envuelto | `[…]` |

Solo se convierte lo que contradice el esquema: un objeto legítimo se deja intacto. Cada
reparación se registra, y si un servidor rechaza los argumentos **a pesar** de la
reparación, lo que se le envió también se registra — sin eso es imposible saber qué forma
les dio el cliente.

## Conectarlo con tu agente de IA

```sh
kling connect                          # guía paso a paso
kling connect eco                      # URL, estado y configuración
kling connect eco -install opencode    # te la escribe
kling connect eco -install claude-code
kling connect -all -install all        # todos los agentes detectados de una vez
```

Siete clientes soportados con `-install`: **Claude Code, opencode, Cursor, VS Code,
Windsurf, Cline y Zed** — o `-install all` para escribir en todos los que detecte.

`connect` **comprueba el servicio de verdad** — hace un `initialize` MCP real y lista las
herramientas — antes de entregarte nada. Una configuración que parece correcta y no
responde es peor que ninguna, porque el fallo aparece dentro del agente y ahí es mucho
más caro de diagnosticar.

```
Service:   eco
Endpoint:  http://192.168.2.60:8080/mcp/eco
Status:    ✓ kindling-echo v1.0.0 · 2 tool(s): echo, session_info
```

Con `-install` hace copia del fichero antes de tocarlo (`.kling-backup`) y conserva el
resto de la configuración. Para Claude Code usa `claude mcp add` si el CLI está
disponible, que es la vía oficial, y solo escribe el JSON si no lo está.

`gateway.url` es la dirección que usan los **agentes** para llegar al gateway, que no
tiene por qué ser la de escucha:

```sh
kling config set gateway.url http://192.168.2.60:8080
```

## Migrar un MCP existente sin romper nada

```sh
kling migrate <mcp> -install <cliente>
```

`migrate` mueve a kindling un servidor MCP que ya usas **conservando el nombre de la
entrada y el de las herramientas** — conecta por el endpoint per-servicio, así que las
skills y prompts que referenciaban `filesystem.read_text_file` siguen funcionando sin
reescribirse. Esa es la diferencia con añadir el servidor a mano y apuntar tu agente al
agregador, donde los nombres cambian.

## Traer tu propio servicio de memoria

Un [volumen](https://github.com/juan52878911/kindling/blob/main/README.es.md#volúmenes-lo-que-sobrevive-a-la-microvm) da almacenamiento durable a un
servicio, pero tiene **un solo escritor**: no puede compartirse en lectura-escritura
entre microVMs (un ext4 montado dos veces en escritura se corrompe solo — NFS o virtio-fs
añadirían mucha maquinaria para algo que un servidor MCP ya resuelve). Para el estado que
deben compartir muchas herramientas y el propio modelo, enlaza un servidor MCP
**externo**:

```sh
kling mcp link engram http://192.168.2.3:9100/mcp -description "memoria compartida"
kling mcp unlink engram
```

No corre en una microVM: se queda donde ya estaba, y kindling solo enruta hacia él.
Aparece en el agregador como un servicio más, así que cualquier herramienta — y el modelo
— puede guardar en él y leer de él.

### Si tu servidor habla stdio

El mismo puente que va dentro de las microVMs funciona en tu máquina:

```sh
make bridge-local
./kling-bridge-local -- engram mcp --tools=agent
kling mcp link engram http://127.0.0.1:9100/mcp
```

Desde v0.4.0 el puente local escucha en `127.0.0.1:9100` **por defecto**: lo que se
envuelve suele ser tu memoria personal, no autentica, y `/reset` quedaría accesible para
cualquiera que alcance el puerto. Si el gateway corre en otra máquina, exponerlo sigue
siendo un flag explícito: `-listen 0.0.0.0:9100`.

## Memoria de uso (opcional)

Apagada por defecto: kindling no escribe en la memoria de nadie sin que se lo pidan. El
binario del puente se instala siempre, eso sí, así que encenderla es un comando y no un
proyecto.

```sh
kling memory status            # si está activa y contra qué
kling memory install-service   # deja el puente local como servicio permanente (macOS)
kling memory enable            # usa engram; -service <svc> para otro
kling memory disable
```

Cuando está activa, el gateway anota en el servicio de memoria qué herramienta resolvió
cada petición, y usa ese historial para ordenar mejor las búsquedas siguientes:

```
buscar "leer un fichero de texto"  →  filesystem.read_text_file
usar la herramienta                →  "hello from kindling"
engram guarda entonces:  kindling: request "leer un fichero de texto"
                         was resolved with tool filesystem.read_text_file
```

No almacena nada propio: se apoya en el servicio MCP que hayas enlazado, y busca en su
catálogo una herramienta de escritura en vez de asumir una API concreta.

## Servidores MCP oficiales corriendo

Los servidores oficiales de Anthropic, alojados como microVMs:

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
kling mcp import filesystem
```

```
SERVICE      TOOLS   CATALOG   HEALTH         MEMORY   INSTANCES
everything   13      6m ago    healthy (6m)   128M     1
thinking     1       3h ago    healthy (1h)   121M     1
memory       9       3h ago    healthy (2h)   122M     1
filesystem   14      3h ago    healthy (1h)   125M     1
notas        2       3h ago    healthy (3h)   42M      1
eco          2       3h ago    healthy (3h)   42M      1
engram       11      2h ago    —              —        external: http://192.168.2.3:9100/mcp

52 tool(s) across 7 service(s) (6 microVM, 1 external).
```

`everything` es el servidor de referencia del protocolo y habla **Streamable HTTP
nativo**: no lleva puente. Los demás hablan stdio y van envueltos. Desde fuera no se
distinguen.

Uso real, a través del agregador y en microVMs de un solo uso:

```
filesystem.read_text_file  /data/test.txt     ->  "hello from kindling"   (31 ms)
memory.create_entities     kindling/project   ->  entidad creada          (31 ms)
everything.get-sum         {"a":100,"b":23}   ->  "The sum … is 123."     (nativo)
```

**31 ms por acción**, cada una en su propia máquina, que muere al terminar.

---

# Operación

## Gateway MCP

Enruta las llamadas a herramientas y las despierta bajo demanda. Corre **separado del
daemon**, a propósito: el daemon nunca escucha en red porque controlarlo equivale a root
en su host. El gateway sí escucha, pero lo único que sabe hacer es despertar instancias
de snapshots que ya existen.

```sh
kling gateway -listen 127.0.0.1:8080 -idle 5m   # genera el token la primera vez

# El gateway EXIGE token: despertar un snapshot es ejecutar código, y el daemon
# se protege no escuchando, pero el gateway sí escucha.
T=$(kling config path >/dev/null && echo "$KLING_GATEWAY_TOKEN")
curl -H "Authorization: Bearer $T" http://127.0.0.1:8080/mcp/echo/
curl -H "Authorization: Bearer $T" http://127.0.0.1:8080/services
curl http://127.0.0.1:8080/healthz              # abierto: es la sonda de vida
```

El token se guarda en `gateway.token` en el host donde corre el gateway, y se copia al
cliente con `kling config set gateway.token …` (`kling connect` lo hace por ti). Para
saltárselo en desarrollo existe `-no-auth`, que insiste en escuchar en loopback. El
gateway **nunca reenvía su propio token** a invitados ni a URLs de terceros — un servidor
MCP comprometido no debe llevarse la credencial del agregador. Cuando un mismo token lo
comparten varios tenants, las **cuotas por token** evitan que uno mate de hambre al
resto.

Medido de punta a punta con un servidor MCP real dentro de la microVM:

| Camino | Latencia |
|---|---|
| Frío (instanciar desde el dorado) | **244 ms** |
| Caliente | **9 ms** |
| Tras congelarse por inactividad | **218 ms** (29 ms de thaw + red del invitado) |

Cuando expira el tiempo de inactividad la herramienta **se congela, no se mata**: deja de
costar CPU y RAM, y la siguiente llamada la trae de vuelta en milisegundos.

### Que sobreviva a los reinicios

```sh
sudo install -m644 packaging/kling-gateway.service /etc/systemd/system/
sudo systemctl enable --now kling-gateway
```

El gateway **no corre como root**: solo habla con el daemon por su socket y proxya. Todo
el trabajo privilegiado se queda en `kling.service`.

### La salud se anota del tráfico real

El gateway ya sabe cuándo un servicio falla — devuelve un 502 — y esa señal ahora **se
graba** en vez de tirarse: `kling mcp health` la enseña, los cambios de estado se
persisten, y un éxito posterior recupera el servicio. Esto existe porque nueve servicios
pasaron una vez 26 horas caídos mientras `status` decía «✓ 9»: informaba del inventario,
y se leía como salud.

### Varios hosts

Un snapshot no viaja entre daemons: un servicio vive en el host donde se importó.
`-hosts` (o la clave de configuración `mcp.hosts`, mismo formato) apunta un gateway a
varios a la vez, en vez de a uno solo:

```sh
kling gateway -hosts mac=unix:///tmp/kling.sock,lab=ssh://juan@192.168.2.60 -listen 0.0.0.0:8080
```

`/mcp/<servicio>` va al host que lo tiene en su catálogo; si lo tienen varios, al que
reporte más memoria disponible, con reintento en el siguiente si ese no tiene sitio (507)
o llegó a su tope de máquinas (409). `/mcp/_all` combina el catálogo de todos los hosts y
enruta cada `call_tool` a su dueño — un host que no contesta se salta, sin tumbar a los
demás. El token, los tenants y las cuotas siguen funcionando igual, comprobados una sola
vez en el enrutador que va delante de todos. **Sin `-hosts`/`mcp.hosts`, nada cambia**: un
solo host, el del contexto activo, como siempre.

## Autocuración: `kling mcp heal`

Un reinicio del host invalida **todos** los snapshots dorados a la vez — Firecracker los
ata a la frecuencia del TSC — y hasta ahora había que reimportarlos a mano. `heal` sondea
cada servicio y reconstruye **solo lo que el TSC invalidó**: un servicio enfermo por otra
causa no se "arregla" rehaciéndolo, y reimportarlo sería ruido que tapa el problema real.
Reconstruye con la configuración **original** del servicio — memoria, vCPUs, egress,
volúmenes, etiquetas — no con la de por defecto.

Ponlo en un temporizador y los reinicios se curan solos:

```sh
sudo cp packaging/kling-heal.service packaging/kling-heal.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kling-heal.timer
# OnBootSec=2min, OnUnitActiveSec=6h
```

## Verificar un servicio, de verdad

```sh
kling mcp verify <servicio>     # -deep es el valor por defecto
```

Una verificación que no puede fallar no es una verificación: `verify` antes pedía
`tools/list` y salía 0 sin ejercitar nada. Ahora **llama a una herramienta real**
(`browser_navigate` sobre `about:blank` en las imágenes de navegador) y consulta el
endpoint `/dns` del puente, que devuelve los nameservers del invitado y si resuelve — así
un servicio con el egress roto o el navegador muerto suspende la comprobación en vez de
aprobarla.

## El puente vive dentro de cada imagen

Es el PID 1 del invitado, así que se copia dentro al construir la imagen. Actualizar
kindling en el host **no** actualiza el puente de los servicios ya empaquetados:

```sh
kling mcp refresh-bridge              # todas
kling mcp refresh-bridge semgrep      # solo una
kling images rm <imagen>          # retirar una imagen que ya no usa nadie
kling images recipe <imagen>      # cómo se construyó
```

Esto no es una función que falta, es un fallo desconcertante si se te olvida: un puente
viejo no entiende los parámetros nuevos de la línea de comandos del kernel, muere al
arrancar y — siendo PID 1 — el invitado entra en pánico. Deducir de un pánico del kernel
que una imagen necesita actualizarse es pedir demasiado.

Nunca toca una imagen que alguna microVM esté usando (modificar un ext4 que otro sistema
tiene montado lo corrompe, aunque ese sistema lo tenga en solo lectura), compara por
contenido para no reescribir lo que ya está al día, y escribe al lado y renombra: o está
el puente viejo o el nuevo, nunca uno truncado. Si el puente ya no cabe, la imagen **se
agranda** en vez de fallar. Refrescar invalida el dorado, y eso **queda grabado como
salud** — el servicio aparece como pendiente de reimportar en vez de romperse en
silencio. En una imagen por capas toca la capa (o, si el puente va horneado en la base,
solo la base — una vez para todos sus servicios).
