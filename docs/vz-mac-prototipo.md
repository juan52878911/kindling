# Prototipo: kindling nativo en Mac con Virtualization.framework

**Pregunta.** ¿Puede kindling correr en un Mac Apple Silicon sin la VM Linux
intermedia (Lima) ni virtualización anidada, usando Virtualization.framework en
lugar de Firecracker, e igualar su eficiencia y su consumo?

**Respuesta corta.** En latencia, sí y con mucho margen: el arranque en frío es 8×
más rápido y el `initialize` de un servidor MCP de node pasa de 9,2 s a 2,3 s. En
memoria, no: una máquina **restaurada** de snapshot ocupa ~350 MiB frente a los
~8 MiB de Firecracker, porque el framework vuelca toda la RAM del invitado al
restaurar y no la mapea perezosamente desde disco. El backend nativo compensa para
uso local (sin Lima, sin virt anidada, y valdría en M1/M2), pero con una política
distinta: arrancar en frío por defecto y mantener pocas calientes.

Medido el 2026-09-21 en un Mac M4 (16 GiB, macOS 26.5.1). El prototipo está en
[`prototypes/vz-mac`](../prototypes/vz-mac/).

## Cómo se midió

Misma configuración en los dos lados, para que la única variable sea el hipervisor:

- **Kernel**: el `vmlinux-6.1.177` aarch64 del CI de Firecracker, el mismo que usa
  el daemon. Ya es un `Image` arm64 (cabecera `ARMd`) y arranca tal cual en
  Virtualization.framework: trae virtio-PCI, virtio-console, globo y vsock.
- **Imágenes**: `min.ext4` con `/sbin/overlay-init` (raíz de solo lectura en `vda`,
  overlay en `vdb`) y `seqthink.layer.ext4`, la capa del servidor
  `@modelcontextprotocol/server-sequential-thinking` construida con
  `80-mcp-image.sh`, montada como `kling.layer=/dev/vdc`.
- **Línea de comandos**: la de `bootArgsBase` cambiando `console=ttyS0` por
  `console=hvc0` y quitando `pci=off`.
- **Máquina**: 1 vCPU, 256 MiB, como el daemon por defecto.
- **Firecracker**: v1.16.1 dentro de la VM Lima `kling-arm` (vz + virt anidada,
  6 vCPU, 8 GiB), lanzado a mano con `--config-file` para la imagen por capas y
  con `kling run/freeze/thaw` para la mínima.
- **Memoria**: en Firecracker, RSS del proceso. En Virtualization.framework,
  `phys_footprint` del proceso auxiliar `com.apple.Virtualization.VirtualMachine`
  que el framework crea por VM **más** el del proceso propio. El invitado vive en
  el auxiliar: medir solo el proceso propio da 18 MiB para una máquina que ocupa 80.

## Imagen mínima: arranque, congelar, descongelar

| | Firecracker anidado (Lima) | Virtualization.framework nativo |
|---|---|---|
| Arranque en frío hasta shell | 2235–2380 ms | 266–317 ms |
| Congelar | 330–587 ms (`kling freeze`) | 105–118 ms, fichero de 11,3 MiB |
| Descongelar | 50–61 ms (`kling thaw`) | 121–159 ms restaurar + 0,1 ms reanudar |
| Primer comando tras descongelar | — | 21–26 ms (≈150 ms de extremo a extremo) |
| Memoria, máquina en marcha | 36 MiB | 80 MiB (62 auxiliar + 18 propio) |
| Memoria tras descongelar | ~8 MiB (Linux, `docs/hallazgos.md`) | **338 MiB** |
| 10 réplicas restauradas | +68 MiB en total (Linux, `docs/hallazgos.md`) | **3,1 GiB** (311 MiB cada una) |
| 10 descongelaciones en paralelo | 50 en 418 ms (Linux) | 10 en 969 ms; en serie, 2006 ms |
| Restaurar según RAM configurada | — | 128 MiB: 121 ms · 512: 150 ms · 1024: 277 ms |

El globo funciona: con dos máquinas que ocupan y liberan 150 MiB en tmpfs, la
huella pasa de 460 a 151 MiB por máquina al fijar el objetivo en 64 MiB, y el
invitado sigue respondiendo. Es el equivalente de `kling squeeze`.

## Servidor MCP real: seqthink (node), `initialize` por HTTP al puente

| | Firecracker anidado (Lima) | Virtualization.framework nativo |
|---|---|---|
| Arranque hasta que el puente escucha | 2725–3189 ms | 599–608 ms |
| Primer `initialize` (node en frío) | **9185–9315 ms** | **2283–2300 ms** |
| `tools/list` en la misma sesión | — | 2,7 ms |
| Memoria tras el `initialize` | 123 MiB | 172 MiB (149 auxiliar + 23 propio) |
| Congelar | — | 136–166 ms, fichero de 41 MiB |
| Restaurar hasta que el puente responde | ~140 ms reanudar (`docs/mac-arm64.md`) | 159–253 ms restaurar + 114–1004 ms hasta responder |
| `tools/list` tras descongelar, sesión del snapshot | — | 4–8 ms |
| Memoria tras restaurar | — | 397 MiB con 256 MiB · 267 MiB con 128 MiB |
| Globo a 128 MiB tras restaurar | — | 281 MiB |
| 5 réplicas restauradas en paralelo | — | 867 ms; 355 MiB cada una; 249 MiB con globo a 128 |

Los 114–1004 ms hasta que el puente responde tras restaurar son jitter de red, no
del restore: la sonda contó hasta 7 conexiones fallidas mientras la NAT del
framework volvía a aprender la MAC del invitado. Con una red por máquina en el
propio proceso (abajo) ese tramo desaparece.

## Lo que se aprende

**1. La latencia del Mac era la virtualización anidada, no kindling.** Mismo
kernel, misma imagen, mismo comando: 9,2 s frente a 2,3 s en el `initialize`, y
2,3 s frente a 0,3 s en el arranque de la mínima. Las palancas de
`docs/mac-arm64.md` (`-bundle`, `-cpu-pct 100`, http-proxy) atacaban el síntoma;
el hipervisor nativo quita la causa.

**2. Restaurar cuesta toda la RAM del invitado.** `vmmap` del auxiliar tras
restaurar: 256 MiB de `VM_ALLOCATE` **sucios al 100 %**, más 69 MiB de
`MALLOC_SMALL` y 17 MiB de `MALLOC_LARGE` propios del framework. Firecracker mapea
el `mem.file` desde disco y solo faultea lo que se toca, que es lo que hace posible
"warm = 0 RAM, thaw = 8 MiB, 10 réplicas = +68 MiB". Virtualization.framework no
ofrece ese camino. Inflar el globo **antes** de congelar no ayuda (el restore
vuelve a comprometerlo todo); inflarlo **después** recorta ~30 %.

**3. Las réplicas heredan su identidad de red del snapshot.** El `ip=` de la
línea de comandos solo se aplica al arrancar; una máquina restaurada conserva la
IP que tenía. Es el mismo modelo que en Linux, donde cada invitado es `172.16.0.2`
en su propio namespace. En macOS la NAT del framework es compartida, así que el
backend real necesita una red **por máquina dentro del proceso**:
`VZFileHandleNetworkDeviceAttachment` con una pila TCP/IP en espacio de usuario
(gVisor netstack). Eso, además, aplica el egress `none | internet | allowlist` en
el propio daemon, sin nftables, y elimina el jitter de ARP.

**4. El estado guardado exige la misma identidad de máquina.** El framework
genera un `VZGenericMachineIdentifier` aleatorio por configuración y se niega a
restaurar con otro ("invalid argument"). Hay que persistirlo junto al estado, como
el `snap.file` lleva la configuración en Firecracker.

**5. Cada VM es un proceso auxiliar de Apple.** Es lo que hace de jailer: el
invitado no corre en el daemon. A cambio son ~90 MiB de overhead por máquina que
Firecracker no paga (su proceso pesa 5–25 MiB).

## Segunda ronda: ¿se puede hacer más eficiente?

Palancas probadas sobre el mismo prototipo, con la memoria restaurada como
objetivo principal.

| Palanca | Resultado | Veredicto |
|---|---|---|
| Pulso de globo tras restaurar (inflar y soltar) | seqthink 256: 397 → 248 MiB inflado a 96 → **411 MiB al soltar**. Mínima ×5: 338 → 121 → 206 | **No sirve.** Al desinflar, el invitado repuebla las páginas y la huella vuelve |
| Globo mantenido tras restaurar | seqthink 256 a 96 MiB: 248 MiB. seqthink 128 a 64: 218 MiB. Mínima a 32: 121 MiB | Sirve, pero la huella nunca baja de objetivo + ~90 MiB del auxiliar |
| `mem_mib` menor | seqthink 128 restaurada: 267 MiB (256: 397) | Proporcional, como en Linux (`densidad: mem_mib es el límite`) |
| **Nivel "pausada en RAM"** (`Pause` sin guardar) | seqthink: **173 MiB estables** durante 30 s; `Resume` + `tools/list` en **7 ms** | **La mejor palanca.** Más barata que restaurar (397 MiB) y 20–100× más rápida (150–1000 ms) |
| 2 vCPU | puente en 419 ms (600 con 1); `initialize` 2250 ms (2290) | El arranque de node es monohilo; no mueve |
| Presión real de memoria (12 restauradas, Mac con 160 MiB libres) | `phys_footprint` suma 3,9 GiB, pero residente 1,7 GiB y compresor +0,8 GiB; sin swap | Las páginas a cero se comprimen casi gratis: **~210 MiB efectivos por máquina**, no 336 |
| 20 restauraciones simultáneas | la #19 falla con "internal Virtualization error" | Hace falta compuerta de arranque, como `KLING_MAX_PARALLEL_BOOT` |
| `-bundle` (esbuild) | no medible: el bundle de `server-sequential-thinking` muere al arrancar con "Could not locate package.json for server version" | Bug de empaquetado de kindling, no del hipervisor: el servidor busca su `package.json` junto al fichero y `80-mcp-image.sh` no lo copia a `/opt` |

**Lo que cambia la política del Mac.** En Linux, la jerarquía de kindling es
`running → warm (fichero, 0 RAM, thaw 30 ms)`. En Virtualization.framework el
restore es la operación cara en memoria, así que la jerarquía natural pasa a tener
un escalón más:

```
running   172 MiB   sirve
paused    173 MiB   7 ms hasta responder      <- lo que hoy es "warm"
saved       0 MiB   150–1000 ms; 397 MiB al volver; globo mantenido: ~250
cold        0 MiB   2,9 s hasta usable; 172 MiB
```

Con esto, la propuesta para el gateway en Mac: pausar al vencer el TTL en vez de
congelar, guardar a disco solo tras un TTL largo o por presión, y al restaurar
inflar el globo y dejarlo. Las N populares (`-keepwarm`) viven pausadas.

## Qué necesitaría un backend real

- Una interfaz de backend en `internal/machine` (hoy no existe: el `Manager` llama
  a `fc.New` directamente) con dos implementaciones, Firecracker y vz.
- Red por máquina con pila en espacio de usuario; MMDS servido por esa misma pila en
  `169.254.169.254`, o por vsock.
- Persistir identificador de máquina y MAC junto al snapshot; clonar el overlay con
  `clonefile` de APFS (el prototipo ya lo hace con `cp -c`).
- Política de memoria distinta a la de Linux: arrancar en frío por defecto (2,9 s
  hasta usable, 172 MiB) y mantener calientes solo las N populares, con globo
  tras descongelar y `mem_mib` ajustado por servicio. Con 350 MiB por máquina
  restaurada, en 16 GiB caben ~40, no 142.
- Sin equivalente directo de `-cpu-pct`: el framework no limita CPU por VM.

## Qué no se midió

Egress y allowlist, volúmenes, MMDS, el modo efímero del gateway y el
comportamiento bajo presión de memoria del anfitrión (las páginas a cero de un
restore se comprimen bien, así que la huella real bajo presión podría ser menor
que `phys_footprint`).

## Notas de laboratorio

- El `min.ext4` de la VM Lima era de agosto y su `overlay-init` ignoraba
  `kling.layer=`; se le instaló el `minimal-init.sh` actual (copia de seguridad en
  `min.ext4.bak-20260921`). Es lo que hace `kling images refresh`.
- El daemon de esa VM es v0.3.0 y no entiende imágenes por capas; por eso la
  comparación de seqthink se hizo con `firecracker --config-file` a mano.
- El fichero `Image` producido con `llvm-objcopy -O binary` sobre el `vmlinux` no
  arranca: el `vmlinux` aarch64 de Firecracker **ya es** un `Image`, y objcopy lo
  rompe. Se usa el fichero original.
