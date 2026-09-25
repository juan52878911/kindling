# Encogido seguro de una imagen ext4: funciones compartidas por
# 70-build-minimal-image.sh, 71-build-glibc-base.sh y 81-base-image.sh.
# Se importa con `. lib-ext4-shrink.sh`, no se ejecuta sola.
#
# `resize2fs -M` sobre un ext4 casi lleno con el inodo de redimensionado por
# defecto puede dejarlo con «Resize inode not valid» (e2fsprogs 1.47.0,
# medido en Ubuntu 24.04 / Lima). Ese inodo solo sirve para crecer EN
# CALIENTE (montado); sin él, un refresco (crecerImagen) sigue creciendo
# igual porque ahí la imagen no está montada. Por eso se crea SIN
# -O resize_inode, y el encogido se hace sobre una COPIA dispersa que solo se
# adopta si `e2fsck` la da por sana: encoger es un ahorro de disco, nunca una
# razón para perder la imagen.

# ext4_fs_sano PATH — dice si el ext4 en PATH está limpio, sin tocarlo.
#
# No basta el código de salida de `e2fsck -fn`: e2fsprogs 1.47.0 sale con 0
# aunque haya contestado «no» a una pregunta sin arreglar (p.ej. «Resize inode
# not valid. Recreate?»), así que también cuenta cualquier pregunta que se
# quedó sin resolver.
ext4_fs_sano() {
  local out
  out=$(e2fsck -fn "$1" 2>&1) || return 1
  ! printf '%s\n' "$out" | grep -q '? no$'
}

# ext4_mkfs_sin_resize_inode DEST — crea un ext4 nuevo en DEST sin el inodo de
# redimensionado en caliente, para poder encogerlo luego con ext4_shrink_safe
# sin el fallo descrito arriba. DEST debe existir ya con el tamaño reservado
# (truncate -s).
ext4_mkfs_sin_resize_inode() {
  mkfs.ext4 -q -F -O '^resize_inode' -E nodiscard "$1"
}

# ext4_shrink_safe PATH — encoge PATH a su contenido real, o lo deja tal cual
# con un aviso si el resultado no pasa e2fsck. Nunca deja PATH dañado ni a
# medio escribir: opera sobre una copia dispersa y solo la adopta si sale
# sana. PATH debe estar desmontado.
ext4_shrink_safe() {
  local path="$1" shrunk="$1.shrink"

  # Comprobar ANTES de encoger: resize2fs se niega a tocar un fs sin
  # comprobar, y encoger uno con errores los empeora.
  e2fsck -fy "$path" >/dev/null 2>&1 || [ $? -lt 4 ] || {
    echo "ERROR: $path quedó irreparable" >&2; return 1; }
  ext4_fs_sano "$path" || { echo "ERROR: $path quedó irreparable" >&2; return 1; }

  rm -f "$shrunk"
  cp --sparse=always "$path" "$shrunk"
  local encogida=0
  if resize2fs -M "$shrunk" >/dev/null 2>&1; then
    local blocks bsize
    blocks=$(dumpe2fs -h "$shrunk" 2>/dev/null | awk -F: '/^Block count/{gsub(/ /,"",$2); print $2}')
    bsize=$(dumpe2fs -h "$shrunk" 2>/dev/null | awk -F: '/^Block size/{gsub(/ /,"",$2); print $2}')
    if [ -n "$blocks" ] && [ -n "$bsize" ]; then
      truncate -s "$((blocks * bsize))" "$shrunk"
      ext4_fs_sano "$shrunk" && encogida=1
    fi
  fi
  if [ "$encogida" = 1 ]; then
    mv -f "$shrunk" "$path"
  else
    rm -f "$shrunk"
    echo "AVISO: no se pudo encoger $path sin dañarla; ocupará más disco del necesario" >&2
  fi
  ext4_fs_sano "$path" || { echo "ERROR: $path no pasa e2fsck" >&2; return 1; }
}
