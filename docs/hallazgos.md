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
