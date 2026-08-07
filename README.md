# kindling

Herramientas MCP serverless sobre microVMs de Firecracker. El objetivo final: coger un
servidor MCP open source cualquiera y convertirlo automáticamente en un servicio que se
levanta bajo demanda, en milisegundos, con aislamiento a nivel de kernel.

> Estado: **fase 1 de 4**. El laboratorio arranca microVMs y el snapshot/restore está
> medido y validado. El gateway todavía no existe.

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
- [ ] **Fase 2** — Red por TAP, y resolver que el estado de red no sobrevive al snapshot
- [ ] **Fase 3** — Un servidor MCP real dentro, hablando Streamable HTTP
- [ ] **Fase 4** — Gateway: enrutar llamada → restaurar → proxy → recoger
- [ ] **Fase 5** — Conversión automática de un MCP open source a microVM

## Notas de campo

Ver [docs/hallazgos.md](docs/hallazgos.md) — cosas que cuestan horas de descubrir por tu cuenta,
como que las URLs de artefactos de todos los tutoriales que hay por internet devuelven 404.
