# kindling

Herramientas MCP serverless sobre microVMs de Firecracker. El objetivo final: coger un
servidor MCP open source cualquiera y convertirlo automáticamente en un servicio que se
levanta bajo demanda, en milisegundos, con aislamiento a nivel de kernel.

> Estado: **fase 3 de 5**. `kling` gestiona microVMs con interfaz tipo docker, con red,
> snapshots dorados y eventos en streaming. Falta el gateway MCP.

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
go build -o kling ./cmd/kling                                  # CLI para tu máquina
GOOS=linux GOARCH=amd64 go build -o kling-linux ./cmd/kling    # daemon para el host KVM
```

En el host con KVM, tras copiar el binario a `/usr/local/bin/kling`:

```sh
sudo install -m644 packaging/kling.service /etc/systemd/system/
sudo systemctl enable --now kling
```

`KLING_SOCKET_USER` en el unit cede el socket al usuario con el que entra el CLI por SSH,
para no tener que ejecutar todo el cliente con sudo.

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
| Imagen base, compartida | 386 MB, una sola vez |
| Por máquina en marcha | **~8.5 MB** |
| Por máquina en `warm` | **~81 MB** |

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
| Ubuntu 24.04 + systemd | 80 MB |
| solo `/bin/sh` | **36 MB** |

Casi la mitad del coste es userspace de Ubuntu que una herramienta efímera no necesita. Un
rootfs mínimo con solo el servidor MCP debería quedarse cerca de esos 36 MB.

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
- [ ] **Fase 3** — Un servidor MCP real dentro, hablando Streamable HTTP
- [ ] **Fase 4** — Gateway: enrutar llamada → restaurar → proxy → recoger
- [ ] **Fase 5** — Conversión automática de un MCP open source a microVM

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
