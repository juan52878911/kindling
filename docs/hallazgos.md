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

## Test de estrés: 10, 20 y 50 microVMs

Medido en la VM del laboratorio (4 cores, 3.9 GB), 256 MiB asignados por máquina,
arranque y congelación en paralelo. **Sin un solo fallo ni OOM en ningún lote.**

| Lote | Arranque | RAM en marcha | RSS | Freeze | Thaw | RAM tras thaw | RSS tras thaw | Load |
|---|---|---|---|---|---|---|---|---|
| 10 | 459 ms | 1167 MiB | 807 MiB | 2.9 s | 54 ms | 404 MiB | 78 MiB | 1.6 |
| 20 | 363 ms | 1378 MiB | 1051 MiB | 12.3 s | 84 ms | 470 MiB | 166 MiB | 6.8 |
| 50 | 2360 ms | 2631 MiB | 2341 MiB | 26.6 s | 418 ms | 653 MiB | 446 MiB | 28.1 |

Tres lecturas:

- **`warm` es gratis en RAM, confirmado a escala.** Con 50 máquinas congeladas la RAM
  usada volvió a la línea base (345 MiB) y no quedaba ni un proceso firecracker.
- **`thaw` escala.** 50 máquinas restauradas en 418 ms, frente a 2.360 ms arrancándolas.
- **`freeze` es el cuello de botella**: 26,6 s para 50, con load 28. Es E/S pura — cada
  máquina vuelca ~110 MB y el perforado los vuelve a leer. Ver más abajo por qué en uso
  real esto casi desaparece.

## Restaurar no solo es más rápido: gasta 3x menos RAM

El hallazgo más importante del test. Misma microVM, medido en `/proc/PID/status`:

| | Arrancada en frío | Restaurada |
|---|---|---|
| VmRSS | 110.996 kB | **33.032 kB** |
| RssAnon | 108.540 kB | 10.188 kB |
| RssFile | 2.452 kB | **22.840 kB** |

Con `backend_type: "File"`, Firecracker **mapea** el fichero de memoria en vez de
reservar memoria anónima. Las consecuencias son grandes:

1. Las páginas se cargan **bajo demanda**: solo entra lo que el invitado toca de verdad.
2. Lo residente es **page cache**, no memoria anónima: el kernel puede desalojarlo bajo
   presión y releerlo del snapshot, sin swap.

Por eso 50 máquinas restauradas ocupan 446 MiB de RSS y las mismas 50 arrancadas en frío
ocupan 2.341 MiB.

## La consecuencia de diseño: snapshot dorado

Si N microVMs restauran del **mismo** fichero de snapshot, mapean las mismas páginas y el
kernel las comparte en page cache. Eso es, en la práctica, lo que se busca con UFFD — y
sale gratis con el backend File, a condición de compartir el snapshot.

Hoy kindling guarda un snapshot por máquina, así que no hay compartición. El siguiente paso
arquitectónico es que **el snapshot sea un artefacto de imagen, no de máquina**: se congela
una vez por herramienta y N instancias restauran de ese fichero. De paso desaparece el
cuello de botella del freeze, porque se congela una vez y no una por instancia.

## Reapuntar discos al restaurar: obligatorio y delicado

El snapshot lleva grabadas las rutas de los discos. Si N instancias restauran del mismo
snapshot sin tocar nada, las N escriben en el overlay de la máquina original.

La salida es cargar con `resume_vm: false`, hacer `PATCH /drives/{id}` con la ruta propia y
después `Resume`. Pero con una condición que no es negociable: **el disco nuevo debe ser una
copia del que se congeló**. El invitado despierta con su estado de montaje en memoria —
superbloque, inodos cacheados, journal— y darle un disco con otro contenido lo corrompe.

Por eso `commit` guarda tres cosas, no dos: estado de la VM, memoria y **una copia del
overlay tal como estaba al congelar**. Y la copia se hace con la microVM pausada, para que
sea coherente con la memoria volcada.

## Medida de la compartición de páginas

Al instanciar 10 máquinas del mismo snapshot dorado:

- suma de RSS de los 10 procesos: **258 MiB**
- crecimiento real de la RAM del sistema: **68 MiB**

Los 190 MiB de diferencia son páginas de page cache que los 10 procesos comparten y que cada
uno contabiliza como propias. **RSS no sirve para medir densidad**: hay que mirar la memoria
del sistema.

Comparado con arrancar 10 en frío, que costaron +824 MiB: **12x de densidad**.

## La red no sobrevive al snapshot: se resuelve con namespaces, no con parches

Firecracker graba el `host_dev_name` de cada interfaz dentro del snapshot, y **no permite
parchearlo** al restaurar (a diferencia de los discos, donde `PATCH /drives` sí funciona).
Con N instancias de un snapshot dorado, las N reclaman el mismo TAP.

La salida es invertir el problema: en vez de dar a cada microVM una red distinta, se les da
a todas **la misma red dentro de su propio namespace**. `tap0`, `172.16.0.2` y la misma MAC
en todas; lo que cambia es el namespace y el enlace veth hacia el host.

Consecuencias prácticas:

- Firecracker debe lanzarse **dentro** del namespace (`ip netns exec`), que es donde vive
  su `tap0`.
- El invitado se configura con el parámetro `ip=` del kernel, sin necesitar ninguna
  herramienta de red en la imagen.
- Cada namespace hace MASQUERADE hacia fuera y DNAT hacia dentro, así que desde el host
  cada máquina tiene una IP distinta aunque todas usen la misma internamente.

## Verificar que responde el invitado y no la interfaz del host

Un ping a la IP del namespace puede contestarlo la propia interfaz veth, no la microVM. La
prueba que discrimina es parar la máquina **dejando el namespace en pie**: si sigue
respondiendo, estabas midiendo el host.

Medido: 0% de pérdida con la microVM viva, 100% con la microVM parada y el namespace
intacto.

## Endurecimiento: tres trampas que costaron un ciclo cada una

**1. `setpriv --clear-groups` y `--groups` son mutuamente excluyentes.** Con `--groups` basta:
fija la lista exacta de grupos suplementarios, que era el objetivo.

**2. Nuestra propia red de enlace cae dentro del rango que bloqueamos.** `172.30.0.0/16` está
dentro de `172.16.0.0/12`, así que la regla anti-red-privada descartaba las RESPUESTAS del
invitado al host y rompía el acceso host→microVM. La solución es aceptar
`ESTABLISHED,RELATED` **antes** de los DROP: no abre nada, porque una conexión que el
invitado inicie hacia una red privada es `NEW` y sigue cayendo.

**3. Tras cargar un snapshot no se pueden añadir dispositivos.** El `PUT /entropy` en la ruta
de restauración fallaba con "not supported after starting the microVM". No hace falta: el
virtio-rng ya viene dentro del snapshot porque la plantilla lo tenía al congelarse.

## Bajar privilegios rompe lo que escribe Firecracker, no lo que lee

Al pasar el VMM a un usuario sin privilegios, `commit` empezó a fallar con "Cannot perform
open on the snapshot backing file: Permission denied". Quien escribe el snapshot es
Firecracker, no el daemon: el directorio destino tiene que ser suyo ANTES de pedírselo.

Regla general para este proyecto: **el daemon crea, el VMM escribe**. Todo lo que Firecracker
vaya a escribir hay que cedérselo; todo lo que solo vaya a leer (imágenes base, snapshots
dorados) se le deja en solo lectura.

## Reiniciar el daemon mataba todas las microVMs

`KillMode` por defecto en systemd es `control-group`: al parar el servicio mata **todo** su
cgroup, y los procesos de Firecracker cuelgan de ahí aunque se lancen con `setsid`.

Con `KillMode=process` solo se detiene el daemon, y al volver `reconcile()` readopta lo que
siga vivo. Para readoptar no basta con mirar si el PID existe —los PID se reciclan—: se
comprueba que la línea de comandos del proceso menciona el socket de ESA máquina.

## Los cgroups de las microVMs no pueden colgar del cgroup del servicio

Primer intento: crear los cgroups bajo `kling.service` con `Delegate=yes`. Falla por la regla
de "no procesos internos" de cgroup v2 — un cgroup no puede tener procesos propios y
controladores delegados a la vez.

Se puede sortear moviendo el daemon a una hoja... y entonces **el siguiente arranque del
servicio falla con `219/CGROUP`**, porque systemd ya no puede colocar su proceso principal en
un cgroup con controladores habilitados. El servicio se recupera al reintentar, pero queda
un arranque fallido en cada reinicio.

La solución es un árbol propio en `/sys/fs/cgroup/kindling`, fuera del de systemd. Cada uno
gestiona el suyo y no hay conflicto.

## Borrar un cgroup justo después de matar el proceso falla

`rmdir` sobre un cgroup con procesos da EBUSY, y entre el `SIGKILL` y la salida real del
proceso pasa un instante. Reintentar durante medio segundo lo resuelve; lo que sobreviva lo
recoge el barrido periódico.

## Al medir fugas de recursos, cuidado con el propio test

Comprobando si `stop` liberaba el namespace medí "1 antes, 1 después" y lo di por roto. En
realidad la máquina que paré ya había liberado el suyo al fallar en el paso anterior, y el
que contaba era el de otra máquina que seguía viva. El arreglo era correcto; el test, no.

## Alpine no trae httpd en su busybox

`/usr/sbin/httpd` no existe en el minirootfs, y tampoco basta con invocar `/bin/busybox
httpd`: el applet **no está compilado**. Da `httpd: applet not found`, el init muere y el
kernel entra en pánico con `Attempted to kill init!`.

La solución es `apk add busybox-extras`. Vale como recordatorio general: en una microVM el
`entrypoint` es PID 1, así que cualquier fallo suyo no es un error de arranque de un
servicio, es un pánico del kernel.

## El gateway debe esperar a que la herramienta escuche

Que la microVM esté `running` no significa que el servidor MCP tenga el puerto abierto. Sin
un sondeo de disponibilidad, la primera petición se come un "connection refused" que el
cliente MCP interpreta como que la herramienta no existe.

`waitReady` sondea el puerto hasta que acepta conexiones. Tras un thaw es casi instantáneo;
en frío da margen al arranque del invitado.

## Tras descongelar, la red tarda más que la CPU

El `thaw` mide 29 ms, pero la primera petición HTTP tras despertar tarda ~218 ms. La
diferencia no es la microVM: es el estado de red que no sobrevive al snapshot —el TAP se
recrea y hay que rehacer ARP—. Sigue siendo aceptable, pero explica por qué el número de
`thaw` y el de latencia de extremo a extremo no coinciden.
