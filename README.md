# kindling

Herramientas MCP serverless sobre microVMs de Firecracker. El objetivo final: coger un
servidor MCP open source cualquiera y convertirlo automáticamente en un servicio que se
levanta bajo demanda, en milisegundos, con aislamiento a nivel de kernel.

> Estado: **fase 2 de 5**. `kling` gestiona microVMs con interfaz tipo docker, local o
> por SSH, con eventos en streaming. Falta la red y el gateway MCP.

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

Un snapshot son ~14 KB de estado más **un fichero de memoria del tamaño de la RAM asignada**.
Diez herramientas a 256 MB son 2.5 GB en reposo. Se controla bajando la RAM por microVM y,
más adelante, usando el backend UFFD en lugar de File para carga perezosa.

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

## Hoja de ruta

- [x] **Fase 1** — Laboratorio: microVM que arranca, snapshot/restore medido
- [x] **Fase 1.5** — `kling`: ciclo de vida, estados, eventos, transporte local y SSH
- [ ] **Fase 2** — Red por TAP, y resolver que el estado de red no sobrevive al snapshot
- [ ] **Fase 3** — Un servidor MCP real dentro, hablando Streamable HTTP
- [ ] **Fase 4** — Gateway: enrutar llamada → restaurar → proxy → recoger
- [ ] **Fase 5** — Conversión automática de un MCP open source a microVM

## Notas de campo

Ver [docs/hallazgos.md](docs/hallazgos.md) — cosas que cuestan horas de descubrir por tu cuenta,
como que las URLs de artefactos de todos los tutoriales que hay por internet devuelven 404.
