package machine

import (
	"path/filepath"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// discoDeConfianza es un disco de datos (volumen o copia de carpeta) tal como
// el daemon lo conoce: su id dentro del VMM, su ruta en el host y el modo.
type discoDeConfianza struct {
	id       string
	path     string
	readOnly bool
}

// rutasDeConfianza arma lo que el daemon manda en kling_repoint: kernel, base
// (rootfs, solo lectura), capa si la hay (svclayer, solo lectura), overlay
// (escritura) y los discos de datos. Con un único disco de datos cuyo id no
// es el heredado, se manda también bajo el id heredado ("volume"): el dorado
// de un solo volumen pudo grabarse con cualquiera de los dos y las entradas de
// más se ignoran (patchVolumeDrive).
//
// Es pura: las rutas salen del estado del daemon, nunca del snap.file.
func rutasDeConfianza(kernel, base, capa, overlay string, vols []discoDeConfianza) *fc.Repoint {
	d := map[string]fc.RepointDrive{
		"rootfs":  {Path: base, ReadOnly: true},
		"overlay": {Path: overlay},
	}
	if capa != "" {
		d[layerDriveID] = fc.RepointDrive{Path: capa, ReadOnly: true}
	}
	for _, v := range vols {
		d[v.id] = fc.RepointDrive{Path: v.path, ReadOnly: v.readOnly}
	}
	if len(vols) == 1 && vols[0].id != legacyVolumeDriveID {
		d[legacyVolumeDriveID] = fc.RepointDrive{Path: vols[0].path, ReadOnly: vols[0].readOnly}
	}
	return &fc.Repoint{Kernel: kernel, Drives: d}
}

// discosDelThaw son los discos de datos de mc que el daemon reconstruye: sus
// volúmenes y, detrás, las copias de carpeta (mismo orden de enganche que en
// el arranque).
func (m *Manager) discosDelThaw(mc *api.Machine) []discoDeConfianza {
	var out []discoDeConfianza
	for i, v := range mc.Volumes {
		id := v.DriveID
		if id == "" {
			id = volumeDriveID(i)
		}
		out = append(out, discoDeConfianza{id: id, path: m.volumePath(v.Name), readOnly: v.ReadOnly})
	}
	for j, p := range m.copyImages(mc) {
		out = append(out, discoDeConfianza{id: volumeDriveID(len(mc.Volumes) + j), path: p, readOnly: true})
	}
	return out
}

// rutasDelThaw: las rutas de confianza de una máquina congelada. Una máquina
// sin imagen no existe en producción (todas nacen de una): sin ella no hay
// rootfs que mandar y se devuelve nil, que es el cuerpo de siempre.
func (m *Manager) rutasDelThaw(mc *api.Machine) (*fc.Repoint, error) {
	if mc.Image == "" {
		return nil, nil
	}
	base, capa, err := m.imageLayer(mc.Image)
	if err != nil {
		return nil, err
	}
	return rutasDeConfianza(m.kernelDe(false), base, capa,
		filepath.Join(m.dir(mc.ID), "overlay.ext4"), m.discosDelThaw(mc)), nil
}
