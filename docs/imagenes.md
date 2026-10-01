# Imágenes sin root: `internal/imagen` y el constructor `debian`

Los constructores de siempre (`base`, `mcp`, `llm`) montan un loopback, hacen
chroot y llaman a apt o apk: necesitan root, `mkfs.ext4` y un núcleo que monte
ext4. El constructor `android` ya lo hacía todo en Go; lo que no era de Android
ahora está en `internal/imagen` y lo usa también un constructor nuevo, `debian`.
Con él se construyen imágenes en el daemon de macOS (kling-vz), en un host sin
loop o en un contenedor, y salen iguales bit a bit con las mismas entradas.

## Qué hay en `internal/imagen`

| Pieza | Qué hace |
|---|---|
| `DebianLock` (`debian_lock.go`, de `go run ./internal/imagen/lockgen`) | la base Debian fijada: `debian:trixie-slim` por digest y 31 `.deb` (`iptables procps iproute2 dmsetup ca-certificates` y dependencias, más las actualizaciones de seguridad o del punto de Debian de lo que ya trae la imagen, como `apt-get upgrade`) con su sha256 y la marca de `snapshot.debian.org` del día |
| `PrepareBase` / `Base.Write` | la base: capas OCI + `.deb` encima, como lo dejaría dpkg (`status`, `.list`, md5sums, conffiles), `iptables` → legacy, `ca-certificates.crt`, adelgazada; luego el init (`minimal-init.sh`), con o sin verity, y el ext4 con 32 MiB de holgura |
| `Debs.Install` / `Debs.Ajustar` | lo mismo sobre cualquier árbol: la base o el `/upper` de una capa (partiendo del `status` de la base) |
| `Debs.Resolve` | resuelve paquetes nuevos (Depends y Pre-Depends, sin Recommends) contra los índices de `snapshot.debian.org` del MISMO instante que la base; da un lockfile |
| `Debs.Fetch` / `FetchVerified` | baja y comprueba por sha256; si `deb.debian.org` no lo tiene (o no contesta) va a `snapshot.debian.org`; tres vueltas |
| `PutAgent`, `Entrypoint` | el agente de invitado (comprobado: ELF de la arquitectura) y el `/entrypoint` de `81-base-image.sh` |
| `Verity`, `VerityInit`, `VerityInfo` | árbol de dm-verity y FEC pegados detrás de la capa (`internal/verity`), el bloque del init que la monta por `/dev/mapper` y lo que va a la receta |
| `Marca` | lo que distingue a cada constructor dentro de la imagen: `android` deja la tabla en `/etc/kindling-android/layer.verity` y el dispositivo `android-layer`; `debian`, en `/etc/kindling/layer.verity` y `kindling-layer` |

Android pasa a usarlo sin cambiar lo que sale: `TestBuild` de `internal/android`
construida con el código de antes y el de ahora y comparada con
`go run ./internal/ext4/ext4cmp -mtime` da los mismos ficheros, modos, dueños,
horas y contenidos salvo tres que dependen del puerto aleatorio del registro de
prueba (`IMAGE.txt`, el UUID de `data.ext4` y la raíz de verity), que también
cambian entre dos ejecuciones del código de antes. `lockgen`, con el resolutor
ya en `internal/deb`, saca el mismo conjunto de paquetes (solo cambia openssl,
que tuvo una actualización de seguridad después del día del lock).

## El constructor `debian`

```sh
cat > py.json <<'EOF'
{"packages": ["python3-minimal"], "verity": true}
EOF
kling image build py -builder debian -spec py.json
kling run -name py -image py -allow-exec
kling exec py -- python3 -c 'print(1+1)'
```

El spec:

| Campo | |
|---|---|
| `packages` | paquetes de trixie (hasta 64), con sus dependencias y sin Recommends |
| `lock` | los `.deb` exactos (`name`, `version`, `url`, `sha256`, `size`): el `built.lock` de una construcción anterior. Con él no se baja ningún índice. Las URL solo pueden ser de `deb.debian.org`, `security.debian.org` o `snapshot.debian.org`: el constructor corre como root en el host del daemon |
| `env`, `service` | como en `81-base-image.sh`: variables que el `/entrypoint` carga de `/etc/kling/env` (0600 de root, no legible por el resto del invitado) y un ejecutable que arranca (y relanza) antes de ceder el PID 1 al agente. Sin `service`, solo el agente |
| `verity`, `fec_roots` | la capa detrás de dm-verity, con 2 raíces de FEC por defecto (0 = sin FEC) |
| `arch`, `base_name` | `amd64` o `arm64` (por defecto la del host; para otra, `KLING_GUEST_AGENT_<arch>`) y el nombre de la base (`<nombre>-base`) |

Salen dos ficheros, como con `android`: la base (`debian:trixie-slim` + la base
fijada + el init) y la capa, con los paquetes pedidos, su `status` de dpkg
completo, el agente, el `/entrypoint` y `/etc/kindling/IMAGE.txt`. Los paquetes
van en la capa y no en la base para que verity cubra lo que se pidió. La base es
de la capa (lleva su tabla): solo se sobrescribe una base que escribió este
constructor (`builder: debian-base` en su receta).

La receta lleva en `built` la imagen de Debian, el snapshot, los paquetes de la
base y los añadidos, el `lock` y, con verity, la raíz, la sal y la tabla. La
identidad de la construcción sale de lo que se fija (el lock, no cómo llegó):
resolver o dar el lock de esa resolución da la misma imagen, bit a bit, con
`SOURCE_DATE_EPOCH` igual.

### Medido (2026-10-01)

| Dónde | Qué | Tiempo | Tamaño |
|---|---|---|---|
| lab (CT 105, amd64), daemon privado | `bash` (ya en slim: capa sin paquetes), en frío (base y sus 28 `.deb` incluidos) | 85–108 s | base 129 MiB, capa 26 MiB (el agente) |
| lab | `bash python3-minimal` + verity, en frío | 55–60 s | capa 39 MiB, 4 paquetes, 331 ficheros; verity 81 bloques de hash + 82 de FEC (0,1 s) |
| lab | lo mismo, caché caliente | 4,9 s | |
| Mac M4 (`kling builder debian`, sin daemon ni root), arm64 | `python3-minimal` + verity, en frío | 75 s | base 157 MiB, capa 38 MiB; `e2fsck -fn` limpio |
| Mac M4 | otra vez, caché caliente | 6,5 s | mismos sha256 de base y capa |

Casi todo el tiempo en frío son descargas: en el laboratorio `deb.debian.org`
daba `TLS handshake timeout` en varios `.deb` de cada construcción (15 s cada uno
antes de ir a snapshot). En el laboratorio la imagen sin verity arranca con el núcleo de
Firecracker de siempre y `kling exec` responde (`bash 5.2.37`); la de verity
arranca con el núcleo de Android (`6.1.140-kindling`, `CONFIG_DM_VERITY`):
`dmsetup status` da `kindling-layer: ... verity V` y `python3` (3.13.5) corre.

### Verity en kling-vz (Mac M4, 2026-10-01)

Primera vez en Virtualization.framework. Núcleo: el de
`scripts/builders/kernel` (6.1.140, ahora con `CONFIG_DM_VERITY`), compilado
en arm64; daemon vz propio y la imagen `python3-minimal` de arriba, con FEC
(2 raíces) y sin él (`"fec_roots": 0`).

| Caso | Resultado |
|---|---|
| capa intacta, con y sin FEC | arranca en frío en 157–197 ms; `dmsetup status` da `kindling-layer: 0 79512 verity V`, la capa está montada desde `/dev/mapper/kindling-layer`, el SHA-256 usa `sha256-ce` y `python3` corre |
| un byte cambiado en el superbloque de la capa, sin FEC | `verity: data block 0 is corrupted`, `mount: can't read superblock on /dev/mapper/kindling-layer`, el init sale y el núcleo entra en pánico: el agente no llega a escuchar |
| el mismo byte, con FEC | `verity-fec: FEC 0: corrected 1 errors` y arranca normal (es para lo que está el FEC) |
| capa intacta con el núcleo de Firecracker CI (6.1.177, sin device-mapper) | `refusing to mount the layer unverified` y pánico |

En vz un pánico del invitado no deja la máquina `failed` como en Firecracker:
`panic=1` reinicia y Virtualization.framework vuelve a arrancar el invitado,
así que la máquina sigue `running` y el pánico se repite (7 en un minuto) hasta
que `-wait-ready` se rinde. La capa nunca se monta sin verificar.

## Límites

- **Verity necesita device-mapper en el núcleo del invitado.** El núcleo de
  kindling (`scripts/builders/kernel`) lo trae desde que `config-common` activa
  `CONFIG_DM_VERITY` (y `check-kernel-config.sh` lo exige); el de Android
  también (`prototypes/android/kernel`). El `vmlinux` de Firecracker CI que usan
  hoy el laboratorio y `kling image copy` no lo trae: con él el init no monta la
  capa sin verificar, se para (`dm-verity on /dev/vdc failed; refusing to mount
  the layer unverified`) y la máquina queda `failed` en Firecracker (en vz, en
  bucle de pánicos; ver arriba). Es lo que se quiere, pero hay que saberlo.
- **El constructor `base` no tiene `verity`**: su base (`min`, Alpine) se comparte
  entre capas y no trae `dmsetup`; la tabla de cada capa no tiene dónde ir.
- **No se ejecutan los scripts de los paquetes** (postinst) ni se regenera
  `/etc/ld.so.cache`. Basta para bibliotecas e intérpretes; un paquete que monta
  su configuración en el postinst (crear usuarios, `update-alternatives`) no
  queda igual que con apt.
- **El resolutor no mira versiones**: vale porque índices y base salen del mismo
  snapshot. Por eso el snapshot no se puede elegir en el spec.
- **El índice se baja sin comprobar la firma de `InRelease`** (OpenPGP no está en
  la biblioteca estándar), como `lockgen`: lo que queda fijado y se revisa es el
  sha256 de cada `.deb`, que va en la receta.
