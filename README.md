# kindling

Herramientas MCP serverless sobre microVMs de Firecracker. El objetivo final: coger un
servidor MCP open source cualquiera y convertirlo automáticamente en un servicio que se
levanta bajo demanda, en milisegundos, con aislamiento a nivel de kernel.

> Estado: **fase 5 de 5** — el circuito completo funciona. `kling` gestiona microVMs con interfaz tipo docker, con red,
> snapshots dorados, aislamiento, eventos, gateway MCP con sesiones y **puente
> stdio→HTTP**: cualquier servidor MCP open source se aloja bajo demanda.

**El invitado se considera hostil**: no se sabe qué servidor MCP se va a alojar. Ver
[SECURITY.md](SECURITY.md) para el modelo de amenaza, las barreras y —sobre todo— lo que
todavía NO está resuelto.

## kling

```
$ kling info
endpoint:     ssh://juan@192.168.2.60
daemon:       0.1.0
KVM:          sí
firecracker:  Firecracker v1.16.1

$ kling run -name mcp-demo
efad9e5f7003  mcp-demo  arrancada en frío en 54 ms

$ kling freeze mcp-demo
efad9e5f7003  warm  (754 ms, 256 MiB en disco)

$ kling ps
ID             NOMBRE     IMAGEN    ESTADO   CPU/MEM    EDAD   ÚLTIMA OP
efad9e5f7003   mcp-demo   default   warm     1/256MiB   17s    freeze 754ms, 256MiB

$ kling thaw mcp-demo
efad9e5f7003  running  (22 ms)
```

### Snapshots dorados

Congela una máquina una vez y instancia N copias que **comparten su memoria**:

```
$ kling commit plantilla golden
golden  snapshot dorado  (80M de memoria)

$ kling run -from golden -name g1
a3f9...  g1  instanciada desde golden en 34 ms

$ kling snapshots
NOMBRE   IMAGEN    CPU/MEM    MEMORIA   DISCO   INSTANCIAS   EDAD
golden   default   1/256MiB   80M       80M     10           21s

$ kling events
23:52:20  machine.frozen   mcp-demo  congelada en 754 ms (256 MiB en disco)
23:52:20  machine.thawed   mcp-demo  descongelada en 22 ms
```

El estado **`warm`** es lo que distingue a kindling de un runtime de contenedores: la
máquina está congelada en disco, no consume CPU ni RAM, y despierta en decenas de
milisegundos.

### Conexión

El mismo binario es CLI y daemon. `kling daemon` corre donde esté KVM; el CLI le habla
por un socket Unix, local o a través de SSH:

```sh
export KLING_HOST=ssh://juan@192.168.2.60   # daemon remoto
export KLING_HOST=/run/kling.sock           # daemon local
```

**El daemon nunca escucha en un puerto de red.** Controlar microVMs equivale a root en su
host: puede montar discos y arrancar kernels arbitrarios. Exponerlo por TCP sería repetir
el error que ha costado a Docker una década de servidores comprometidos. Para remoto se usa
SSH con la misma técnica que `docker context`: en vez de exigir socat o nc en el destino,
se invoca `kling dial-stdio`, que puentea la tubería SSH con el socket local.

### Instalación

```sh
make install                              # CLI en tu máquina
make deploy HOST=ssh://juan@192.168.2.60  # daemon en el host con KVM
```

`make install` elige el primer directorio escribible de tu PATH y **no pide sudo**:
instalar una herramienta de usuario no debería requerirlo. Fuérzalo con
`make install PREFIX=/usr/local` si lo prefieres en el sistema.

`make deploy` compila para `linux/amd64`, copia binario y unit de systemd, y arranca el
servicio. El unit cede el socket al usuario con el que entras por SSH, para no ejecutar
todo el cliente con sudo.

### Configuración

Contextos con nombre, al estilo de `docker context`, para no arrastrar `KLING_HOST`:

```sh
kling context add lab ssh://juan@192.168.2.60 -description "Proxmox de casa"
kling context use lab
kling context ls
```

Y valores por defecto, para no repetir opciones en cada `run`:

```sh
kling config set defaults.image min
kling config set defaults.ttl_seconds 600
kling config set gateway.idle 5m
kling config show
```

El fichero vive en `~/.config/kling/config.json` — también en macOS: `UserConfigDir()`
devolvería ahí `~/Library/Application Support`, que es correcto para apps de escritorio pero
sorprendente para un CLI.

**Precedencia:** `-H` > `$KLING_HOST` > contexto activo > socket local. El flag gana siempre,
para que una invocación puntual no obligue a cambiar de contexto.

## Por qué microVMs y no contenedores

Un servidor MCP es un proceso Node o Python de 50-100 MB. Meterlo en una microVM no ahorra
recursos frente a un contenedor — cuesta más, porque cada microVM arranca su propio kernel.

La razón de hacerlo es otra: **una IA local ejecutando tooling open source arbitrario es
código no confiado**. El aislamiento de un contenedor es el namespace del kernel compartido;
el de una microVM es una frontera de hipervisor. Esa es la única justificación honesta del
proyecto, y conviene tenerla clara antes de escribir una línea más.

## Números medidos

Medidos sobre Proxmox (Intel i7-8700T) con Firecracker v1.16.1 **anidado** dentro de una VM,
kernel 6.1.177 y rootfs Ubuntu 24.04 de 800 MB:

| Operación | Tiempo |
|---|---|
| Arranque en frío | **2.643 ms** |
| Creación de snapshot | 305 ms |
| **Restauración desde snapshot** | **~30 ms** |

Reprodúcelos con `scripts/40-bench-boot.sh`.

**La conclusión que define la arquitectura:** 2.6 s en frío hace inviable el modelo de una
microVM por petición. Los 125 ms que anuncia Firecracker son con kernel recortado y rootfs
mínimo sobre metal desnudo. Con snapshot/restore, 30 ms es imperceptible dentro de una
llamada de herramienta.

Por tanto: **cada herramienta arranca una vez, se congela con el servidor MCP ya escuchando,
y el gateway restaura bajo demanda.** El snapshot no es una optimización opcional; es lo que
sostiene todo lo demás.

## Coste en disco

La imagen base **no se copia**: se monta en solo lectura y la comparten todas las microVMs.
Cada máquina solo tiene un overlay disperso propio, montado con overlayfs por
`/sbin/overlay-init` dentro del invitado.

| | |
|---|---|
| Imagen base `min` (Alpine), compartida | **17 MB**, una sola vez |
| Imagen base `default` (Ubuntu), compartida | 386 MB |
| Por máquina en marcha | **~8 MB** |
| Por máquina en `warm`, imagen `min` | **~35 MB** |
| Por máquina en `warm`, imagen `default` | ~82 MB |

Antes de los overlays cada máquina copiaba los 800 MB enteros: tres máquinas costaban
2.4 GB, ahora cuestan 386 MB + 25 MB.

**Una máquina `warm` no consume RAM.** `freeze` mata el proceso de Firecracker; lo que
queda es un fichero. Su coste es disco, no memoria.

Firecracker vuelca la memoria entera al congelar, pero la mayor parte son páginas a cero.
kindling las perfora con `fallocate --dig-holes`: el kernel devuelve ceros al leer un
agujero, que es exactamente lo que había, así que la restauración no se entera.

**256 MB → 81 MB, y el `thaw` sigue en ~30 ms.**

### Qué determina ese coste

Dos medidas que orientan cualquier optimización futura:

| RAM asignada | Coste congelada |
|---|---|
| 512 MiB | 86 MB |
| 256 MiB | 81 MB |
| 96 MiB | 80 MB |

**Asignar más RAM es casi gratis** una vez el fichero es disperso: lo que se guarda es el
working set real, no la RAM reservada. Bajar `-mem` no es la palanca.

La palanca es lo que arranca dentro:

| Invitado | Coste congelada |
|---|---|
| Ubuntu 24.04 + systemd | 82 MB |
| Alpine sin systemd (imagen `min`) | **35 MB** |

Casi la mitad del coste era userspace de Ubuntu que una herramienta efímera nunca usa.
`scripts/70-build-minimal-image.sh` construye la imagen `min`: Alpine con
`/sbin/overlay-init` y sin gestor de servicios, que arranca directamente `/entrypoint`.

Más allá quedan dos técnicas de Firecracker sin explotar: **snapshots diff**, que guardan
solo las páginas cambiadas respecto a una base, y el **backend UFFD**, que permite a varias
microVMs restauradas del mismo snapshot compartir páginas en RAM. UFFD no reduce disco,
pero es lo que da densidad cuando hay muchas herramientas calientes a la vez.

## Arquitectura

```
   Modelo local (Qwen3)
            │  MCP / Streamable HTTP
            ▼
      ┌───────────┐   restaura snapshot (~30 ms)
      │  gateway  │──────────────┬──────────────┐
      └───────────┘              ▼              ▼
                            ┌────────┐    ┌────────┐
                            │ µVM: A │    │ µVM: B │
                            └────────┘    └────────┘
```

El gateway recibe la llamada de herramienta, restaura el snapshot que corresponde, hace de
proxy de la petición y recoge la microVM cuando expira su TTL.

## Requisitos

- Un host con KVM y `cpu: host` (o equivalente) para que pasen las extensiones de virtualización
- Si corre anidado, virtualización anidada activada en el host padre
- `firecracker` + `jailer`, `e2fsprogs`, `squashfs-tools`, `curl`, `jq`

Sobre **macOS**: Firecracker no corre nativo. En Apple Silicon M3 o superior con macOS 15+
puede correr dentro de una VM Linux aarch64 con anidación. Sirve para desarrollar, no como
runtime — consume más batería que la solución que este proyecto pretende evitar.

## Scripts

| | |
|---|---|
| `scripts/10-provision-lab.sh` | Crea la VM del laboratorio en Proxmox |
| `scripts/20-install-firecracker.sh` | Instala Firecracker y jailer desde la última release |
| `scripts/30-fetch-artifacts.sh` | Descubre y descarga kernel + rootfs del CI |
| `scripts/40-bench-boot.sh` | Mide arranque en frío, snapshot y restauración |
| `scripts/50-prepare-image.sh` | Inyecta `overlay-init` y registra la imagen base |

## Hoja de ruta

- [x] **Fase 1** — Laboratorio: microVM que arranca, snapshot/restore medido
- [x] **Fase 1.5** — `kling`: ciclo de vida, estados, eventos, transporte local y SSH
- [x] **Fase 1.6** — Overlays, snapshots dispersos y snapshots dorados con memoria compartida
- [x] **Fase 2** — Red por TAP con un namespace por microVM
- [x] **Fase 2.5** — Endurecimiento: privilegios bajados, salida filtrada, límites de caudal
- [x] **Fase 2.6** — Imagen mínima, TTL, cgroups de CPU, reconciliación y vigilancia
- [ ] **Fase 3** — Un servidor MCP real dentro, hablando Streamable HTTP
- [x] **Fase 4** — Gateway: enrutar llamada → restaurar → proxy → recoger por inactividad
- [x] **Fase 5** — Puente stdio→HTTP: cualquier servidor MCP se aloja bajo demanda

## Notas de campo

Ver [docs/hallazgos.md](docs/hallazgos.md) — cosas que cuestan horas de descubrir por tu cuenta,
como que las URLs de artefactos de todos los tutoriales que hay por internet devuelven 404.

## Densidad: por qué el snapshot dorado lo cambia todo

Un snapshot dorado es un artefacto **de imagen, no de máquina**: se congela una vez y N
instancias restauran del mismo fichero. Como Firecracker lo **mapea** en vez de reservar
memoria anónima, el kernel comparte esas páginas entre todas las instancias y cada una solo
paga lo que escribe.

Medido instanciando de una en una y mirando la RAM del sistema:

| | 10 desde snapshot dorado | 10 arrancadas en frío |
|---|---|---|
| RAM total añadida | **+68 MiB** | +824 MiB |
| Por máquina | **6.8 MiB** | 82 MiB |
| Tiempo por máquina | ~40 ms | ~2.6 s hasta userspace |

**12 veces más densidad.** La prueba de que las páginas se comparten está en el desfase
entre dos cifras: la suma de RSS de los diez procesos daba 258 MiB, pero la RAM del sistema
solo subió 68 MiB. Los 190 MiB de diferencia son páginas compartidas que cada proceso
cuenta como suyas.

Esto es, en la práctica, lo que se persigue con UFFD — y sale del backend `File`, sin
escribir un gestor de fallos de página.

## Red: un namespace por microVM

```
$ kling topo
kindling  ssh://juan@192.168.2.60
          KVM ok · Firecracker v1.16.1

  host  172.30.0.0/16
   ├─◆ golden           snapshot dorado · 82M de memoria compartida
   │  ├── g3             running  172.30.0.18       384K  thaw 28ms
   │  ├── g2             running  172.30.0.14       384K  thaw 26ms
   │  └── g1             running  172.30.0.10       384K  thaw 41ms
   │
   └─◆ (arrancadas en frío)
      └── plantilla      running  172.30.0.6          8M  boot 46ms

  4 running · 0 warm · 0 parada(s)   disco: 9M propio + 83M compartido
```

**El problema:** un snapshot graba el nombre del dispositivo TAP del host. Si N instancias
restauran del mismo snapshot dorado, las N piden el mismo TAP y chocan. Y no vale
reasignarlo: Firecracker no permite parchear `host_dev_name`.

**La solución:** un namespace de red por microVM. Dentro de cada uno el TAP se llama
siempre `tap0` y el invitado tiene siempre la misma IP, así que **el snapshot vale para
todas**. La diferenciación ocurre entera en el host, al otro lado de un veth:

```
        host                  │  netns kl-<id>          │  microVM
 vh-<id> 172.30.a.b/30 ◄─veth─► vg-<id> 172.30.a.b+1    │
                              │ tap0    172.16.0.1/30   ├─ eth0 172.16.0.2
```

El invitado se configura con el parámetro `ip=` del kernel, sin necesitar herramientas de
red dentro de la imagen. Desde el host cada máquina se alcanza por la IP de su namespace,
que hace DNAT hacia el invitado.

Es el mismo enfoque que usa AWS Lambda, y por la misma razón.

## Aislamiento

El invitado es código de terceros: se asume hostil.

| Barrera | Cómo |
|---|---|
| Daemon inalcanzable por red | Solo socket Unix; remoto exclusivamente por SSH |
| VMM sin privilegios | `setpriv` a un usuario de servicio: **CapEff 0**, `no_new_privs`, solo grupo `kvm` |
| Sin acceso a la LAN | Salida `none` por defecto; con `internet` las redes privadas siguen bloqueadas |
| Sin degradar a los vecinos | 128 MiB/s de disco y 16 MiB/s de red por máquina; tope de 256 |
| Claves no repetidas | virtio-rng + `CONFIG_VMGENID`: el invitado resiembra al restaurar |

Verificado **desde dentro del invitado**, que es la única medición que vale:

```
RESULTADO 192.168.2.100: BLOQUEADO      (host Proxmox)
RESULTADO 192.168.2.1:   BLOQUEADO      (router de casa)
RESULTADO 10.10.10.1:    BLOQUEADO      (túnel WireGuard)
RESULTADO 169.254.169.254: BLOQUEADO    (metadatos de cloud)
RESULTADO 1.1.1.1:       ALCANZABLE
```

## Ciclo de vida y robustez

```
$ kling run -image min -ttl 300 -cpu 25 -egress internet
$ kling logs <ref> -tail 50        # consola serie: la única ventana al interior
```

- **`-ttl`** congela la máquina sola pasado ese tiempo. Congelar, no matar: deja de costar
  CPU y RAM, pero vuelve en ~30 ms. Es lo que hace serverless al modelo.
- **`-cpu`** acota el uso de CPU con un cgroup propio (50% de un core por defecto).
- **Reconciliación al arrancar**: el daemon compara su estado guardado con la realidad del
  host, readopta las microVMs que sigan vivas y limpia namespaces y cgroups huérfanos.
- **Vigilancia continua**: cada 10 s comprueba que lo que dice `running` corre de verdad.
  Una máquina cuyo proceso desapareció pasa a `failed` y libera sus recursos.

**Reiniciar el daemon no mata las microVMs.** El unit lleva `KillMode=process`; sin eso
systemd arrastra todo el cgroup y se lleva por delante las máquinas en marcha.

### Medido con 8 instancias

```
RAM añadida:          113 MiB   (14 MiB por instancia)
conectividad:         9/9
VMM sin privilegios:  9/9
cgroups activos:      9
```

## Gateway MCP

Enruta llamadas a herramientas y las despierta bajo demanda. Corre **aparte del daemon**, a
propósito: el daemon nunca escucha en red porque controlarlo equivale a root en su host. El
gateway sí escucha, pero solo sabe despertar instancias de snapshots que ya existen.

```sh
kling gateway -listen 127.0.0.1:8080 -idle 5m
curl http://127.0.0.1:8080/mcp/echo/       # la herramienta aparece sola
curl http://127.0.0.1:8080/services        # inventario y qué está caliente
```

Medido de extremo a extremo con un servidor MCP real dentro de la microVM:

| Camino | Latencia |
|---|---|
| En frío (instanciar del snapshot dorado) | **244 ms** |
| Caliente | **9 ms** |
| Tras congelarse por inactividad | **218 ms** (29 ms de thaw + red del invitado) |

Al agotarse el tiempo de inactividad la herramienta **se congela, no se mata**: deja de
costar CPU y RAM, y la siguiente llamada la trae de vuelta en milisegundos.

### Envolver un servidor MCP

```sh
sudo ./scripts/80-mcp-image.sh mi-tool ./mi-servidor "nodejs npm"
kling run -name tmpl -image mi-tool -service mi-tool
kling commit tmpl mi-tool && kling stop tmpl
```

El directorio necesita un `entrypoint` ejecutable que escuche en el puerto 8080. Ver
[examples/echo](examples/echo).

## Informe HTML

```sh
kling export -o topologia.html      # se genera en TU máquina, no en el daemon
```

Autocontenido: sin CDN, sin fuentes remotas, sin peticiones al abrirlo — describe la
topología de un homelab y no tiene por qué contársela a nadie. Agrupa por la etiqueta
`service`, y en su defecto por el snapshot de origen, porque dos máquinas del mismo snapshot
comparten memoria y pertenecen juntas aunque nadie las haya etiquetado.

```sh
kling run -from echo -service echo -label tier=prod
```

## Convertir cualquier servidor MCP en un servicio

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data

kling mcp import filesystem
```

`mcp import` hace el ciclo entero:

```
1/5  arrancando la plantilla...        ✓ 0aeee853 en 172.30.0.6
2/5  esperando al servidor MCP...      ✓
3/5  preguntando qué sabe hacer...     ✓ 14 herramienta(s)
4/5  congelando como snapshot dorado...✓
5/5  guardando el catálogo...          ✓
```

El paso 3 es **introspección**: se le pregunta al servidor qué sabe hacer una sola vez, y el
paso 5 guarda ese catálogo junto al snapshot.

**`-n` preinstala los paquetes npm en la imagen.** No es comodidad: las microVMs arrancan sin
salida a internet, así que un `npx -y` en tiempo de ejecución fallaría al descargar.

## El inventario no toca el servicio

Sin catálogo persistido, preguntar "¿qué herramientas hay?" obliga a un `tools/list` contra
cada servidor, y eso **despierta sus microVMs**. Una pregunta de inventario acabaría
arrancando veinte máquinas.

Con `mcp import`, el catálogo vive en disco junto al snapshot:

```sh
kling mcp list -v          # todas las herramientas, sin arrancar nada
kling mcp refresh <svc>    # recapturar tras actualizar el servidor
```

Verificado: listar el inventario completo por el agregador deja el contador de máquinas
en **0**.

## Modo efímero: una microVM por acción

```sh
kling gateway -ephemeral -prewarm 3
```

Cada llamada recibe **su propia microVM**: se toma una del fondo pre-calentado, atiende la
acción y se destruye. Nace, actúa y muere.

```
acción 1: 19 ms   pid=305 llamadas_en_esta_sesion=1
acción 2: 24 ms   pid=305 llamadas_en_esta_sesion=1
acción 3: 19 ms   pid=305 llamadas_en_esta_sesion=1
```

`llamadas_en_esta_sesion=1` **en todas**: ninguna acción ve lo que hizo la anterior.

### De 350 ms a 19 ms

Perfilando una acción efímera sin optimizar:

| Fase | Coste |
|---|---|
| Restaurar la microVM | 131 ms |
| Esperar a que vuelva la red | 53 ms |
| `initialize` (lanza el servidor MCP) | 61 ms — **con node son 300-500 ms** |
| `tools/call` | 9 ms |
| Destruir la máquina | ~100 ms |

Todo menos `tools/call` se puede pagar por adelantado o después:

- **`-prewarm N`** mantiene N instancias ya restauradas y **con su sesión MCP abierta**. La
  llamada se ahorra restaurar, esperar la red e inicializar.
- **La destrucción es asíncrona.** Estaba en un `defer`, así que el cliente esperaba al
  desmontaje del namespace y al borrado de ficheros: 100 ms sobre una llamada de 2 ms. La
  máquina muere igual; el cliente ya no espera a que ocurra.

Resultado: **2 ms de ejecución real, 19 ms de extremo a extremo.**

La contrapartida sigue siendo que no hay estado entre llamadas. Las herramientas que lo
necesitan —memoria, razonamiento por pasos— deben usar la ruta con sesión
(`/mcp/<servicio>`), que mantiene el proceso vivo.

La contrapartida es que no hay estado entre llamadas. Las herramientas que lo necesitan
—memoria, razonamiento por pasos— deben usar la ruta con sesión (`/mcp/<servicio>`), que
mantiene el proceso vivo.

## Fase 5: cualquier servidor MCP, alojado bajo demanda

La mayoría de servidores MCP open source solo hablan **stdio**: un proceso hijo persistente
con el que se dialoga por tuberías. No hay puerto al que llamar, y el ciclo de vida lo impone
el cliente. Es lo contrario de invocable bajo demanda.

`kling-bridge` corre **dentro** de la microVM, lanza el servidor como hijo y expone su
protocolo por Streamable HTTP:

```
gateway ──HTTP──> kling-bridge ──stdin/stdout──> servidor MCP
```

Desde fuera, un servidor stdio parece nativo de HTTP. Envolverlo es una línea:

```sh
make bridge
sudo ./scripts/80-mcp-image.sh stdio files -p "nodejs npm" -- \
     npx -y @modelcontextprotocol/server-filesystem /data

kling run -name files-tmpl -image files -service files
kling commit files-tmpl files && kling stop files-tmpl
```

Para servidores que ya hablan HTTP el modo es `http` y no hace falta puente.

### Sesiones

MCP identifica conversaciones con `Mcp-Session-Id`, y un servidor stdio es de **sesión única
por naturaleza**: su estado vive en el proceso. Por eso:

- **El puente lanza un proceso hijo por sesión.** Dos conversaciones concurrentes no se
  pisan el estado.
- **El gateway enruta de forma pegajosa.** La misma sesión vuelve siempre a la misma
  microVM; mandarla a otra instancia encontraría un servidor sin ese estado.

Demostrado con la herramienta `session_info`, que informa de su pid y de sus llamadas:

```
sesión 1 (3 llamadas extra): pid=305 llamadas_en_esta_sesion=5
sesión 2 (recién creada):    pid=309 llamadas_en_esta_sesion=1
```

### El circuito completo

```
modelo local  ──>  gateway  ──>  microVM  ──>  servidor MCP
  (tu Mac)        (Proxmox)     (Firecracker)   (stdio o HTTP)
```

[examples/agent/agent.py](examples/agent/agent.py) lo cierra: cliente MCP + bucle de
tool-calling contra ollama.

```
$ python3 examples/agent/agent.py "usa echo para decir hola"
→ kindling-echo v1.0.0  sesión b2787e00
→ herramientas: echo, session_info
→ llamando echo({"text": "hola"})
← hola
```

El modelo no sabe nada de microVMs: pide una herramienta y aparece. Si llevaba rato sin
usarse estaba congelada, y despertarla cuesta milisegundos.

| Camino | Latencia |
|---|---|
| Handshake MCP en frío, desde el Mac | **310 ms** |
| Llamada a herramienta, en caliente | **9 ms** |

## Conectarlo con tu agente de IA

```sh
kling connect                          # guía paso a paso
kling connect eco                      # URL, estado y configuración
kling connect eco -install opencode    # la escribe por ti
kling connect eco -install claude-code
```

`connect` **comprueba el servicio de verdad** —hace un `initialize` MCP y lista las
herramientas— antes de darte nada. Una configuración que parece correcta y no responde es
peor que ninguna, porque el fallo aparece dentro del agente y ahí cuesta mucho más
diagnosticarlo.

```
Servicio:  eco
Endpoint:  http://192.168.2.60:8080/mcp/eco
Estado:    ✓ kindling-echo v1.0.0 · 2 herramienta(s): echo, session_info
```

Con `-install` respalda el fichero antes de tocarlo (`.kling-backup`) y conserva el resto de
la configuración. Para Claude Code usa `claude mcp add` si el CLI está disponible, que es la
vía oficial, y solo escribe el JSON si no lo está.

`gateway.url` es la dirección por la que los **agentes** alcanzan el gateway, que no tiene
por qué ser la de escucha:

```sh
kling config set gateway.url http://192.168.2.60:8080
```

### Que sobreviva a los reinicios

```sh
sudo install -m644 packaging/kling-gateway.service /etc/systemd/system/
sudo systemctl enable --now kling-gateway
```

El gateway **no corre como root**: solo habla con el daemon por su socket y hace de proxy.
Toda la parte privilegiada se queda en `kling.service`.

## Una sola entrada para todos los servicios

Un cliente MCP carga las definiciones de **todas** las herramientas al conectarse. Con veinte
servicios de diez herramientas son doscientos esquemas JSON en el contexto del modelo antes
de que empiece a trabajar.

```sh
kling connect -all -install opencode              # todos los servicios
kling connect -all -only eco,notas -install opencode   # solo algunos
kling connect -all -expand                        # catálogo completo
```

El endpoint `/mcp/_all` es un servidor MCP que enruta a los demás. Tiene dos modos:

**`proxy`** (por defecto) — expone **cuatro meta-herramientas** en vez de N:

| | |
|---|---|
| `list_services` | qué servidores hay y cuántas herramientas tiene cada uno |
| `find_tools` | busca por palabras clave; devuelve nombres y descripciones, **sin esquemas** |
| `describe_tool` | el esquema completo de una sola herramienta |
| `call_tool` | ejecuta, enrutando a la microVM que toque |

El modelo busca lo que necesita, pide el esquema de lo que va a usar, y llama.

**`expand`** — aplana el catálogo con nombres `servicio.herramienta`, para clientes que
funcionen mejor con todo cargado.

### Cuál sale más barato depende de cuántas herramientas tengas

El modo `proxy` tiene un **coste fijo** de ~300 tokens; `expand` crece con cada herramienta.
Con pocas herramientas, proxy sale **más caro**. Por eso `connect -all` lo mide contra tu
catálogo real y te lo dice:

```
Coste en contexto, con tu catálogo actual:
  proxy    3 definiciones  ≈  248 tokens
  expand  28 definiciones  ≈ 4327 tokens
  → El modo proxy que estás usando ahorra 4079 tokens.
```

El punto de cruce está en torno a 8 herramientas. Con 28 el proxy ahorra **17 veces**; con
200, decenas de miles de tokens en cada conversación.

## Servidores MCP oficiales corriendo

Los servidores oficiales de Anthropic, alojados como microVMs efímeras:

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
kling mcp import filesystem
```

```
SERVICIO     HERRAMIENTAS   CATÁLOGO    MEMORIA   INSTANCIAS
filesystem   14             46s atrás   125M      0
memory        9             2m atrás    122M      0
```

Uso real, a través del agregador y en microVMs de un solo uso:

```
filesystem.read_text_file  /data/prueba.txt   ->  "hola desde kindling"   (31 ms)
memory.create_entities     kindling/proyecto  ->  entidad creada          (31 ms)
```

**31 ms por acción**, cada una en su propia máquina que muere al terminar.

### Tres cosas que hay que saber al empaquetar un servidor de node

- **`-n` preinstala el paquete npm.** Las microVMs arrancan sin salida a internet: un
  `npx -y` fallaría al descargar.
- **El `entrypoint` es PID 1 y el kernel no le da PATH.** Sin fijarlo, los binarios que
  instala npm no se encuentran: `executable file not found in $PATH`.
- **Los directorios que espera el servidor deben existir DENTRO de la imagen.** El
  `server-filesystem` quiere `/data`; crearlo en el host no sirve de nada.

## Efímero o persistente: se decide solo

Una microVM efímera muere con todo lo suyo, memoria **y disco**. Así que la pregunta no es
"¿el servidor guarda estado?" sino:

> ¿algo que escribe una llamada tiene que verlo una llamada posterior?

`kling mcp import` lo deduce del catálogo y lo dice:

```
eco          EFÍMERO      porque solo consulta: no deja nada que preservar
notas        PERSISTENTE  porque escribe con guardar_nota y lee con session_info
filesystem   PERSISTENTE  porque escribe con write_file y lee con read_file
memory       PERSISTENTE  porque read_graph sugiere que acumula contexto
thinking     PERSISTENTE  porque sequentialthinking sugiere que acumula contexto
```

**`filesystem` también es persistente**, aunque no lo parezca: escribe en el disco del
invitado, que es tan volátil como su memoria.

La señal fiable es estructural —que el servidor exponga a la vez herramientas que escriben y
que leen—, con un puñado de palabras en el NOMBRE de la herramienta para los servidores de
una sola. Buscar esas palabras en las descripciones clasificaba todo como persistente:
"session" o "sequence" aparecen de pasada en cualquier texto.

Se puede forzar con `-stateful` o `-ephemeral`.

### Ante la duda, persistente

Equivocarse hacia efímero produce **pérdida silenciosa**: la llamada responde bien y lo
escrito desaparece. Equivocarse hacia persistente solo cuesta una instancia congelada, que
no gasta ni CPU ni RAM.

El agregador lo indica en su inventario para que el modelo lo sepa:

```
memory [recuerda entre llamadas]: create_entities, add_observations, ...
filesystem: read_text_file, write_file, list_directory, ...
```

## El inventario va en el handshake

Los nombres de herramienta son baratos —27 ocupan ~100 tokens—; lo caro son los esquemas de
argumentos. Por eso el `initialize` devuelve el **inventario completo** en su campo
`instructions`:

```
Herramientas disponibles, agrupadas por servicio:

filesystem: read_text_file, write_file, list_directory, ...
memory [recuerda entre llamadas]: create_entities, ...

Llámalas con call_tool y el nombre completo servicio.herramienta.
Si no conoces sus argumentos, pide primero describe_tool.
```

Así el modelo sabe qué hay **desde el primer momento** y va directo a `call_tool`, en vez de
gastar una llamada en descubrir. Quedan tres meta-herramientas: `find_tools`,
`describe_tool` y `call_tool`.

### Persistente no significa siempre encendido

Un servicio persistente conserva su estado, pero **deja de consumir al terminar**. Medido con
`-idle 30s`:

```
escribir /data/nota.txt        →  Successfully wrote
leer /data/nota.txt (otra llamada) →  "esto debe sobrevivir"

en marcha:   running   41 MiB sobre la línea base
tras 35 s:   warm      RAM de vuelta a 0   (freeze 661 ms)
al volver:   "esto debe sobrevivir"        (thaw 18 ms)
```

Congelar no es apagar: la instancia deja de existir como proceso —cero CPU, cero RAM— pero su
estado sigue en disco y vuelve en milisegundos. El coste es el fichero de memoria: 151 MiB
para este servicio mientras está congelado.
