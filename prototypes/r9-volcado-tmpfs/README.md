# Spike r9-volcado-tmpfs · volcar a tmpfs y copiar disperso hasheando (C9-02, variante M)

Prototipo medible de la ronda 9 (Z7). **No toca el producto**: usa el arnés de
`../r9-hugepages/lib-fc.sh` (Firecracker a mano).

## Hipótesis

El commit de un dorado recorre el `mem.file` tres veces en disco: Firecracker escribe la RAM configurada
entera (ceros incluidos), `fallocate --dig-holes` la relee y `digest.Tree` relee lo asignado. En un gp3 de
125 MiB/s son ~30 s para von-q4 (1600 MiB). Volcando a un tmpfs y pasando al disco con una copia que salta
las páginas a cero y hashea a la vez, se escribe una sola vez lo asignado y no se relee nada: ~27 → ~11 s.

De paso responde lo que C9-02 pide confirmar antes de tocar el producto: cuánto queda sucio y cuánto en la
caché de página justo después del volcado de Firecracker 1.17 (`sucio_tras_volcado_mib`,
`en_cache_tras_volcado_mib`).

## Cómo se corre

    IA_PRUEBAS=s-volcado IA_MODELOS=von-q4=qwen2.5-1.5b-instruct:q4_k_m BINS_DIR=… scripts/bench/ec2.sh ia

o `bash spike.sh <dir>` como root (`DESTINO` = el disco a medir, `/var/tmp` por defecto).

Por vuelta (`VUELTAS`, 2) y variante, alternando, con `drop_caches` antes:

- `hoy`: volcado Full al disco, `fallocate --dig-holes`, fsync y relectura hasheando (saltando huecos).
- `m`: volcado Full a un tmpfs (`MEM_MIB` + 64 MiB de RAM transitoria) y copia dispersa con hash al disco,
  en un pase, con fsync.

Cifras: ms de cada paso y total, bytes escritos al disco (`/sys/block/*/stat`) y lo asignado; al final,
`cmp` de los dos resultados (deben ser iguales: un hueco lee como ceros).

## Resultado

`resultado.json` con `recomendacion`: **seguir** si `m` tarda ≤ 60 % de `hoy` con el mismo contenido;
**descartar** si no (quedarse con el pase único, C9-02 S). La copia está en Python: es una cota superior
de lo que haría el producto en Go. Pendiente de correr.

## Límites

La RAM transitoria es igual a `MemMiB` por cada fabricación simultánea: en producto necesita admisión de
memoria y caer al camino de hoy si no cabe. El jail del producto no se reproduce aquí.
