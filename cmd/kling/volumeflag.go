package main

import (
	"fmt"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// volumeFlag acumula los -volume de la línea de comandos.
//
// Se acepta más de uno porque los dos usos naturales se estorban: un servicio
// quiere su almacenamiento propio en escritura Y la biblioteca de paquetes
// compartida en lectura. El ORDEN en que se escriben es el orden de los discos.
//
// Sintaxis:  nombre  ·  nombre:/punto/de/montaje  ·  nombre:/punto:ro
//
// La forma corta de un solo volumen (-volume X -mount Y -volume-ro) se conserva
// por compatibilidad: scripts de v0.1.0 y CLIs antiguos que hablan con un daemon
// nuevo. NO se documenta a propósito — documentar dos sintaxis para lo mismo
// obliga a quien lee a decidir cuál es la buena. Se combinan en volumeSet().
type volumeFlag []api.VolumeAttachment

func (f *volumeFlag) String() string {
	names := make([]string, len(*f))
	for i, v := range *f {
		names[i] = v.Name
	}
	return strings.Join(names, ",")
}

func (f *volumeFlag) Set(s string) error {
	if s == "" {
		return fmt.Errorf("empty volume")
	}
	v := api.VolumeAttachment{}
	// Se parte por la DERECHA para el modo, y luego por la izquierda para el
	// nombre: así un punto de montaje puede llevar dos puntos si algún día hace
	// falta, sin que el nombre se lo coma.
	rest := s
	if r, ro := strings.CutSuffix(rest, ":ro"); ro {
		rest, v.ReadOnly = r, true
	} else if r, rw := strings.CutSuffix(rest, ":rw"); rw {
		rest = r
	}
	v.Name, v.Mount, _ = strings.Cut(rest, ":")
	if v.Name == "" {
		return fmt.Errorf("missing name in %q; use name[:/mount/point][:ro]", s)
	}
	if v.Mount != "" && !strings.HasPrefix(v.Mount, "/") {
		return fmt.Errorf("mount point of %q must be an absolute path: %q", v.Name, v.Mount)
	}
	*f = append(*f, v)
	return nil
}

// volumeSet combina los -volume repetibles con la forma corta.
//
// Si se usan las dos, manda la lista: -mount y -volume-ro solo tienen sentido
// cuando hay exactamente un volumen, y aplicarlos a uno de la lista sería
// adivinar a cuál.
func volumeSet(vols volumeFlag, mount string, readOnly bool) ([]api.VolumeAttachment, error) {
	if len(vols) == 0 {
		return nil, nil
	}
	if len(vols) > 1 && (mount != "" || readOnly) {
		return nil, fmt.Errorf("-mount and -volume-ro only work with a single volume; with multiple "+
			"volumes put the mount point on each one:  -volume %s:/path", vols[0].Name)
	}
	out := append([]api.VolumeAttachment(nil), vols...)
	if len(out) == 1 {
		if mount != "" && out[0].Mount == "" {
			out[0].Mount = mount
		}
		if readOnly {
			out[0].ReadOnly = true
		}
	}
	return out, nil
}
