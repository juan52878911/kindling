package main

// Montaje de los volúmenes persistentes dentro de la microVM.
//
// El puente es PID 1: corre antes que nadie y es el único sitio donde se puede
// montar sin tocar el init de la imagen base. Ponerlo en el init obligaría a
// reconstruir TODAS las imágenes existentes para estrenar volúmenes; aquí no
// hace falta reconstruir ninguna.
//
// Los puntos de montaje llegan por la línea de comandos del kernel
// (kling.volume=/data,/libs:ro) en vez de estar grabados en la imagen, así que
// el mismo snapshot dorado sirve con volúmenes distintos, o montados en otro
// sitio.
//
// El montaje en sí vive en volume_linux.go: syscall.Mount no existe fuera de
// Linux, y sin separarlo un `go build ./...` en un Mac —donde se desarrolla— no
// compilaría.

import (
	"os"
	"strings"
	"sync"
)

// volumeBootParam debe coincidir con api.VolumeBootParam. Se repite aquí como
// literal a propósito: el puente se compila estático para el invitado y no debe
// arrastrar el paquete api solo por una constante.
const volumeBootParam = "kling.volume"

// firstVolumeDevice es el primer disco de volumen. vda es la base de solo
// lectura y vdb el overlay de la máquina; los volúmenes empiezan en vdc y
// siguen por orden de enganche.
const firstVolumeDevice = 'c'

// volumeSpec es un volumen a montar: dónde y cómo.
type volumeSpec struct {
	device   string
	mount    string
	readOnly bool
}

// volumeSpecs lee de /proc/cmdline qué montar y en qué modo.
//
// El valor es una lista separada por comas —"/data" o "/data,/libs:ro"— y la
// POSICIÓN es el contrato: el i-ésimo punto de montaje corresponde al i-ésimo
// disco de volumen, contando desde /dev/vdc. Por eso no se ordena ni se filtra
// nada aquí: alterar el orden montaría cada volumen en el sitio de otro.
//
// El modo va pegado a cada punto de montaje, y no como parámetro aparte, para
// que sea imposible leer uno sin el otro: montar en escritura lo que se pidió
// de solo lectura corrompería lo que están leyendo las demás microVMs.
func volumeSpecs() []volumeSpec {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil
	}
	var raw string
	for _, tok := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(tok, volumeBootParam+"="); ok && v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	var out []volumeSpec
	for i, spec := range strings.Split(raw, ",") {
		if spec == "" {
			continue
		}
		v := volumeSpec{device: "/dev/vd" + string(rune(firstVolumeDevice+i))}
		if mp, ro := strings.CutSuffix(spec, ":ro"); ro {
			v.mount, v.readOnly = mp, true
		} else {
			v.mount = spec
		}
		out = append(out, v)
	}
	return out
}

// volumeState guarda si los volúmenes están montados ahora mismo.
//
// Hace falta estado porque el montaje deja de ser una cosa que pasa al arrancar:
// el daemon puede pedir DESMONTAR antes de congelar un snapshot dorado y volver
// a MONTAR después de restaurarlo. Sin llevar la cuenta, un acquire de más
// montaría dos veces sobre el mismo punto —tapando el primero— y un release de
// más devolvería un error donde no lo hay.
//
// Es global y con candado propio porque lo tocan dos manejadores HTTP a la vez y
// el apagado, y ninguno pasa por el mutex del puente.
var volumeState = &volumes{}

type volumes struct {
	mu      sync.Mutex
	mounted []volumeSpec
}

// specs devuelve lo montado. Copia, para que quien la reciba no pueda alterar el
// estado por debajo.
func (v *volumes) specs() []volumeSpec {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]volumeSpec(nil), v.mounted...)
}

// acquire monta lo que pida el kernel. Es idempotente: si ya está montado, no
// hace nada.
func (v *volumes) acquire() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.mounted) > 0 {
		return nil
	}
	got, err := mountVolumes()
	if err != nil {
		return err
	}
	v.mounted = got
	return nil
}

// sync vacía al disco lo que haya en la caché del invitado.
func (v *volumes) sync() {
	v.mu.Lock()
	defer v.mu.Unlock()
	syncVolumes(v.mounted)
}

// release desmonta. También idempotente: desmontar dos veces no es un error, y
// el apagado ordenado puede llegar después de un release del daemon.
func (v *volumes) release() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.mounted) == 0 {
		return
	}
	unmountVolumes(v.mounted)
	v.mounted = nil
}
