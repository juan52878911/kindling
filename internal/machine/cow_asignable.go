package machine

// Espacio ASIGNABLE del almacén de copia al escribir.
//
// statfs no basta para decidir si una instancia nueva cabe. En Btrfs el
// espacio se reparte en chunks de datos y de metadatos: un almacén puede
// tener cientos de MiB libres dentro de sus chunks de datos y fallar igual con
// ENOSPC porque los metadatos se llenaron y ya no queda nada sin asignar para
// otro chunk (reproducido en el laboratorio: almacén de 1 GiB, 222 MiB libres
// en datos, 1 MiB sin asignar, metadatos al límite de la reserva global; el
// invitado vio "I/O error, dev vdb" y Postgres hizo PANIC). Al revés también
// pasa: el statfs de Btrfs es una estimación.
//
// La cuenta, con los datos de BTRFS_IOC_SPACE_INFO (sin herramientas
// externas):
//
//	sin asignar = tamaño del dispositivo − Σ chunks asignados
//	datos       = (asignado − usado en datos) + sin asignar
//	metadatos   = (asignado − usado en metadatos) − reserva global + sin asignar
//
// Lo asignable es lo de datos, salvo que los metadatos estén por debajo de
// metaMinimaAlmacen: entonces cualquier escritura que necesite un extent nuevo
// (todas, en un clon) puede fallar, y lo asignable es 0. Y nunca más de lo que
// dice statfs: ante la duda, la cifra menor.

// Banderas de btrfs_ioctl_space_info.flags (include/uapi/linux/btrfs_tree.h).
const (
	btrfsGrupoDatos     = 1 << 0
	btrfsGrupoSistema   = 1 << 1
	btrfsGrupoMetadatos = 1 << 2
	btrfsReservaGlobal  = 1 << 49
)

// metaMinimaAlmacen es lo que tiene que quedar para metadatos en un Btrfs
// para dar el espacio de datos por usable.
const metaMinimaAlmacen = 16 << 20

// grupoBtrfs es una entrada de BTRFS_IOC_SPACE_INFO.
type grupoBtrfs struct {
	flags, total, usado uint64
}

// asignableBtrfs calcula el espacio asignable de un Btrfs a partir de su
// tamaño (statfs), lo libre según statfs y los grupos de SPACE_INFO.
func asignableBtrfs(tam, libreStatfs int64, grupos []grupoBtrfs) int64 {
	var asignado, datosLibre, metaLibre, reserva int64
	mixto := false
	for _, g := range grupos {
		if g.flags&btrfsReservaGlobal != 0 {
			reserva = int64(g.total)
			continue
		}
		asignado += int64(g.total)
		libre := int64(g.total) - int64(g.usado)
		switch tipo := g.flags & (btrfsGrupoDatos | btrfsGrupoMetadatos | btrfsGrupoSistema); {
		case tipo == btrfsGrupoDatos|btrfsGrupoMetadatos:
			mixto = true
			datosLibre += libre
			metaLibre += libre
		case tipo&btrfsGrupoDatos != 0:
			datosLibre += libre
		case tipo&btrfsGrupoMetadatos != 0:
			metaLibre += libre
		}
	}
	sinAsignar := max(tam-asignado, 0)
	datos := datosLibre + sinAsignar
	meta := metaLibre - reserva + sinAsignar
	if mixto {
		datos -= reserva
	}
	if meta < metaMinimaAlmacen {
		return 0
	}
	return max(min(datos, libreStatfs), 0)
}
