# Notas de campo

Cosas descubiertas a golpes, para no repetirlas.

## Los artefactos del CI cambiaron de sitio

Todos los tutoriales y buena parte de la documentación apuntan a:

```
https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.x/${ARCH}/vmlinux-...
```

**Esa ruta devuelve 404.** El bucket pasó a directorios por fecha:

```
firecracker-ci/AAAAMMDD-<hash>-0/${ARCH}/
```

No hay un alias `latest`, así que hay que listar el bucket y quedarse con el más reciente.
`scripts/30-fetch-artifacts.sh` lo hace. El listado sí es público:

```bash
curl -s "https://s3.amazonaws.com/spec.ccfc.min/?list-type=2&prefix=firecracker-ci/&delimiter=/"
```

Cada build publica varios kernels (`vmlinux-5.10.x`, `6.1.x`, `6.18.x`, y variantes
`-no-acpi`) y rootfs en **squashfs** (`ubuntu-24.04.squashfs`, `amazonlinux-2023.squashfs`).

## El rootfs viene en squashfs, que es de solo lectura

Hay que convertirlo a ext4 si quieres escribir dentro:

```bash
unsquashfs -d /tmp/rootfs ubuntu-24.04.squashfs
mkfs.ext4 -F -d /tmp/rootfs rootfs.ext4 800M
```

## La anidación cuesta, y se nota

El arranque en frío de 2.6 s no es representativo de Firecracker sobre metal desnudo. Se
reparte entre la penalización de la anidación, un kernel 6.1 completo con todos sus drivers y
un rootfs Ubuntu sin recortar. Un kernel a medida y un rootfs mínimo bajarían esto mucho —
pero como el snapshot/restore resuelve el problema por otra vía, no es prioritario.

## Pendientes conocidos, documentados por AWS

Dos cosas que hay que resolver en la fase 2, y que conviene no descubrir en producción:

1. **La entropía se clona con el snapshot.** Restaurar el mismo snapshot muchas veces
   reproduce el mismo estado del RNG del invitado. Firecracker lo documenta como problema de
   seguridad si dentro hay criptografía. Hay que re-sembrar tras cada restauración.
2. **El estado de red no sobrevive.** El TAP se recrea y el invitado despierta con tablas ARP
   y conexiones TCP obsoletas. Es la parte tediosa de la fase 2.

## Medir arranques: cuidado con el propio medidor

`timeout N firecracker ... | grep -m1 -q PATRON` **no mide lo que parece**. `grep -q` sale al
primer match pero Firecracker sigue escribiendo, y la tubería no termina hasta que el
`timeout` lo mata: acabas midiendo tu propio timeout, no el arranque.

Para medir en serio, mete en el rootfs un init que se apague solo y cronometra el proceso
entero:

```sh
#!/bin/sh
echo FC_READY
/sbin/reboot -f
```

y arranca con `init=/fcinit.sh`.

## Los 54 ms de `kling run` no son lo mismo que los 2.6 s del benchmark

`kling run` reporta ~54 ms, pero eso es tiempo de **plano de control**: configurar la
microVM por la API y aceptar `InstanceStart`. Firecracker devuelve en cuanto arranca la
vCPU, y el invitado sigue booteando de forma asíncrona.

Los 2.643 ms del benchmark miden otra cosa: hasta que el userspace del invitado está listo.

Son dos métricas distintas y no hay que confundirlas. Para el gateway MCP la que importa es
la segunda, porque una herramienta no sirve hasta que su proceso escucha. Es otra razón
para que el flujo real sea arrancar una vez, congelar con el servidor ya escuchando, y
restaurar: el `thaw` sí devuelve una máquina inmediatamente utilizable.

## El overlay: de 800 MB a 8.5 MB por máquina

Firecracker no tiene capas como Docker, pero el kernel del CI trae `CONFIG_OVERLAY_FS=y`,
así que la solución está dentro del invitado, no en el host:

- `/dev/vda` = imagen base, **`is_read_only: true`**, compartida por todas las microVMs
- `/dev/vdb` = overlay disperso propio de cada máquina
- `init=/sbin/overlay-init` monta el overlayfs y hace `pivot_root` antes de ceder a systemd

Detalles que importan al crear el overlay:

- `-E nodiscard` en `mkfs.ext4`, o mke2fs escribe ceros y destruye la dispersión.
- `-O ^has_journal`: el journal cuesta varios MB de suelo en **cada** máquina y no aporta
  nada en almacenamiento efímero.

Medido con tres máquinas en marcha: base compartida de 386 MB y 8.5 MB por máquina, frente
a 800 MB por máquina antes.

**El coste se movió, no desapareció.** Ahora lo caro es el fichero de memoria del snapshot,
que pesa lo mismo que la RAM asignada (256 MB por máquina warm). Ese es el siguiente
objetivo, y la vía es el backend UFFD en lugar de File.

## `du` miente con ficheros dispersos

`ls -lh` da el tamaño lógico; con overlays dispersos la diferencia con lo realmente
asignado es de dos órdenes de magnitud. `kling ps` suma `st_blocks * 512`, que es la única
cifra honesta.

## El snapshot de memoria es disco, no RAM

Conviene decirlo claro porque induce a error: una máquina `warm` **no ocupa RAM**. `freeze`
pausa, vuelca y **mata el proceso de Firecracker**. Lo que queda es un fichero.

## Perforar el fichero de memoria: 256 MB -> 81 MB

Firecracker vuelca el rango de memoria entero, pero en una microVM recién arrancada la
mayoría son páginas a cero. `fallocate --dig-holes` las convierte en agujeros. Leer un
agujero devuelve ceros, que es justo lo que había, así que la restauración es indiferente:
medido en ~30 ms antes y después, en ciclos repetidos de freeze/thaw.

## Bajar la RAM asignada NO es la palanca

Contraintuitivo, y ahorra perder el tiempo optimizando lo que no toca:

| RAM asignada | Congelada |
|---|---|
| 512 MiB | 86 MB |
| 256 MiB | 81 MB |
| 96 MiB  | 80 MB |

Una vez el fichero es disperso, lo que se guarda es el working set real. La RAM reservada
de más son ceros, y los ceros son agujeros.

## Lo que sí manda: qué arranca dentro

Mismo kernel, misma RAM asignada (256 MiB), distinto init:

| Invitado | Congelada |
|---|---|
| Ubuntu 24.04 + systemd | 80 MB |
| solo `/bin/sh` | **36 MB** |

Casi la mitad del coste es userspace de Ubuntu que una herramienta efímera nunca usa. El
camino para bajar de 80 MB es un rootfs mínimo, no tocar parámetros de memoria.

## Sin explotar todavía

- **Snapshots diff** (`snapshot_type: "Diff"`): guardan solo las páginas cambiadas respecto
  a una base. Con N herramientas sobre una misma imagen, cada diff serían pocos MB.
- **Backend UFFD** en vez de File: varias microVMs restauradas del mismo snapshot pueden
  compartir páginas en RAM. No reduce disco; da densidad cuando hay muchas calientes a la
  vez. Es la técnica con la que Lambda consigue su densidad.
