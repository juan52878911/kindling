# Spike r9-hugepages · pesos mapeados a 2 MiB (C9-18, variante 1)

Prototipo medible de la ronda 9 (`analisis/ronda9-carga/plan-r9.md`, zona Z7). **No toca el producto**:
arranca su propia microVM de Firecracker con `lib-fc.sh`, sin kindling.

## Hipótesis

La primera petición tras restaurar un dorado de VON cuesta ~900 ms en la EC2 anidada, y casi todo son
fallos de EPT: la réplica toca ~1 GiB de pesos en páginas de 4 KiB (~261 000 salidas de VM, C9-07). Si
el `mem.file` del dorado vive en un tmpfs montado con `huge=always`, los fallos de lectura de un
`MAP_PRIVATE` sobre shmem podrían mapearse por PMD y KVM reflejarlos en la EPT a 2 MiB: ~600 salidas en
vez de 261 000 y la primera pasada por los pesos a una fracción.

Lo que no se sabe y el spike responde: si Firecracker mapea el `mem.file` alineado a 2 MiB, si un
`MAP_PRIVATE` de shmem con THP llega a mapearse por PMD en este kernel, y si KVM lo aprovecha
(`pages_2m` del debugfs de KVM).

## Cómo se corre

En el host del banco (root, `/dev/kvm`), lo lanza el banco de la EC2:

    IA_PRUEBAS=s-huge IA_MODELOS=von-q4=qwen2.5-1.5b-instruct:q4_k_m BINS_DIR=… scripts/bench/ec2.sh ia

o a mano: `bash spike.sh <dir de salida>` (`spike.sh --comprobar` solo mira los requisitos).

1. Rootfs de prueba: busybox estático + `tocar` (`tocar.c`, compilado estático en el host) + `/init`.
2. La VM (1600 MiB, 2 vCPU: lo de von-q4) copia 1065 MiB de pesos (el GGUF del modelo si hay uno en
   `KLING_ROOT/cache/von/models`, si no aleatorio) a un tmpfs suyo: RAM del invitado, como `--load-mode dio`.
3. Volcado Full; el mismo `mem.file` en el disco (variante `disco`, lo de hoy) y copiado a un tmpfs
   `huge=always` (variante `tmpfs_huge`).
4. `VUELTAS` restauraciones de cada una, alternando, con la caché caliente: `tocar` lee un byte de cada
   página de 4 KiB de los pesos dos veces (la segunda es la referencia sin fallos).

Por muestra: `peticion_ms` (desde el host), `invitado_ms`, salidas de VM de ese VMM y `pages_4k` /
`pages_2m` (debugfs de KVM), y `ShmemPmdMapped`+`FilePmdMapped` del host.

## Resultado

`resultado.json` en el dir de salida, con `recomendacion`: **seguir** si `pages_2m > 0` y la primera
pasada baja al menos a la mitad; **descartar** si no. Pendiente de correr (la corrida la lanza el
coordinador; este README se completa con la cifra entonces).

## Límites

- En producto exigiría C9-09 (réplicas sin seguimiento de páginas sucias), resolver el enlace duro al
  jail (otro sistema de ficheros) y una copia en disco para persistir; y es RAM no desalojable (como el
  anclado de C9-10).
- La anidada de la EC2 añade su propia EPT (L0); el spike mide lo que ve el L1.
