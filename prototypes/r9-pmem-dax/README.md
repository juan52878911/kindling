# Spike r9-pmem-dax · pesos por virtio-pmem con DAX (C9-18, variante 2)

Prototipo medible de la ronda 9 (Z7). **No toca el producto**: Firecracker a mano con el arnés de
`../r9-hugepages/lib-fc.sh` y, si hace falta, un kernel de invitado **de prueba** compilado aparte
(`config-amd64-pmem` sobre una copia de `scripts/builders/kernel`); el kernel de producto no cambia.

## Hipótesis

Con los pesos en un dispositivo virtio-pmem de solo lectura montado con DAX, no entran en la RAM del
invitado: el dorado de von-q4 pasa de ~1240 a ~200 MiB, el commit escribe ~6× menos, hay una sola copia en
disco y en RAM, y dorados distintos del mismo modelo la comparten.

## Etapas (cada una corta con su cifra o su motivo)

1. **¿Lo admite Firecracker 1.17?** Se busca `pmem` en la especificación de la API del release
   (`$KB/firecracker-api.yaml`, la instala `ia.sh preparar`) y se prueba `PUT /pmem/p0` en una VM sin
   arrancar. Si no → `descartar` con la respuesta de la API como evidencia. `spike.sh --comprobar` hace
   solo esto.
2. **Kernel de prueba** con `VIRTIO_PMEM`, `LIBNVDIMM`, `ZONE_DEVICE`, `FS_DAX` (`VARIANT=pmem`). Si no
   compila o no quedan las opciones → `sin cifra` con el log.
3. **Medida**: la misma VM con los pesos en RAM (copia a tmpfs, como hoy) y con los pesos en un ext4 por
   pmem montado `dax`: volcado Full (ms, MiB escritos, `mem.file` asignado) y restaurar + primera pasada
   por los pesos (`tocar`: ms, salidas de VM).

## Cómo se corre

    IA_PRUEBAS=s-pmem PLAZO_SPIKE_MIN=45 IA_MODELOS=von-q4=qwen2.5-1.5b-instruct:q4_k_m BINS_DIR=… scripts/bench/ec2.sh ia

`VMLINUX_PMEM=<kernel ya hecho>` se salta la etapa 2.

## Resultado

`resultado.json` con `recomendacion`: **seguir** si el volcado con pmem escribe ≤ 1/3 que con RAM;
**descartar** si no o si Firecracker no lo admite. Pendiente de correr.

## Límites

Aunque salga bien, en producto exige un kernel de invitado nuevo (invalida todos los dorados) y llama.cpp
con mmap sin reempaquetar (`dio` existe porque con mmap se duplicaba). La medida de la etapa 3 es con
`tocar`, no con llama-server: da el coste de poblar la EPT, no el de una petición real.
