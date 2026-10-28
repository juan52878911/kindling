# hvf-cow: RAM del invitado como fichero MAP_PRIVATE en Hypervisor.framework

Spike de viabilidad: ¿puede un VMM propio sobre Hypervisor.framework (HVF) dar en
Mac el restore perezoso copy-on-write que Firecracker tiene en Linux? Hoy
Virtualization.framework carga toda la RAM del snapshot (324 MiB por instancia).

`hvcow.c` crea una VM con HVF, mapea un fichero de 256 MiB con
`mmap(MAP_PRIVATE)` + `hv_vm_map` como RAM del invitado y arranca un vCPU (EL1,
MMU apagada) con código aarch64 propio que lee una palabra por página de 16K,
escribe un bloque y sale con `HVC #0`. Mide `task_info(TASK_VM_INFO)`,
`mincore()`, `mach_vm_region`, `footprint` y `vmmap`. El modo `alloc` es el
control: `hv_vm_allocate` + copiar el fichero entero (lo que hace VZ).

## Cómo correrlo

```sh
./build.sh                       # clang + firma ad-hoc con com.apple.security.hypervisor
./run.sh /ruta/temporal 6        # todas las pruebas, 6 procesos en la prueba de compartición
./hvcow run -t -r 1 -w 64 FICHERO  # una ejecución suelta (ver opciones en hvcow.c)
```

Sin sudo. Los ficheros se escriben con `F_NOCACHE` para que la primera lectura
del invitado venga del disco y no de la caché.

## Resultados (MacBook Pro M4 (Mac16,1), 16 GiB, macOS 26.5.1, 2026-10-27)

Medido con la máquina en uso normal (poca memoria libre y compresor activo),
así que las cifras de todo el sistema (`vm_stat`) tienen ruido; las del proceso
no.

### (a) Fallos perezosos desde el fichero: SÍ

| etapa (leer 1 MiB, escribir 64 KiB) | phys_footprint | resident | mincore incore |
|---|---|---|---|
| tras `mmap` + `hv_vm_map` | 1,53 MiB | 5,69 MiB | 0 / 16384 |
| tras ejecutar el invitado | 1,88 MiB | 7,03 MiB | 72 / 16384 |

- `hv_vm_map` de 256 MiB tarda 0,008 ms y no hace residente nada; `mmap` 0,01 ms.
- Solo entran las páginas tocadas: 64 leídas + 4 escritas (+4 de la copia).
  Leyendo 128 MiB entran 8204 páginas (128,2 MiB), sin lectura anticipada apreciable.
- La suma que calcula el invitado coincide con el contenido del fichero.
- Coste de un fallo: fichero frío 128 MiB en 627 ms (≈77 µs por página de 16K,
  ~200 MiB/s); con el fichero en caché 11,7 ms (≈1,4 µs por página).

### (b) Escrituras copy-on-write sin tocar el fichero: SÍ

- 64 KiB escritos = 4 páginas: `mincore` marca `copied=4 mod=4`, `vmmap` muestra
  64K sucios en el `mapped file`, `phys_footprint` +0,35 MiB.
- La escritura es visible en el mapeo del anfitrión y el SHA-256 del fichero no
  cambia (`2a47217b…`).

### (c) N mapeos del mismo fichero comparten las páginas limpias: SÍ

6 procesos escalonados sobre el mismo fichero frío, cada uno lee 192 MiB y
escribe 64 KiB y sigue vivo mientras arrancan los demás:

| | modo file (MAP_PRIVATE) | modo alloc (control) |
|---|---|---|
| `hv_vcpu_run` del 1.º | 746 ms (lee del disco) | 13 ms (ya copiado) |
| `hv_vcpu_run` del 2.º al 6.º | 15–22 ms (sin E/S) | 13–20 ms |
| internal (anónima) por proceso | ~1,5 MiB | 258 MiB |
| suma de phys_footprint | 11,7 MiB | 1547,7 MiB |
| suma de resident | 1187 MiB (todo `external`) | 1430 MiB |
| `File-backed pages` del sistema | +219 MiB con el 1.º, sin crecer con los demás | — |

Los procesos 2.º a 6.º fallan 192 MiB a velocidad de caché caliente y su memoria
residente es toda `external` (páginas del vnode), no copias privadas: hay una
sola copia física en la caché unificada. En el control cada instancia paga
258 MiB y, bajo presión, el compresor crece (+638 MiB en la prueba); en modo file
las páginas limpias se pueden descartar y volver a leer sin comprimir ni ir a swap.

## Qué no se ha probado

- Invitado con MMU encendida (granulo de 4K del invitado frente a página de 16K
  del anfitrión: la unidad de copia será 16K).
- Desalojo por el anfitrión de una página ya mapeada en stage-2 y nuevo fallo.
- Dispositivos, GIC, temporizadores y guardado/restauración del estado del vCPU.
