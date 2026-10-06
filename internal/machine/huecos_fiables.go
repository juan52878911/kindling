package machine

// ¿Distingue este sistema de ficheros un hueco de una página a cero?
//
// Un diferencial (diff_volcado.go) depende de eso: un hueco es "como en el
// dorado" y una página de ceros es "el invitado la puso a cero". Hay sistemas
// de ficheros que no lo distinguen y la copia despertaría con la memoria rota
// sin que nada fallara:
//
//   - los que guardan los bloques a cero como huecos (ZFS con compresión, el
//     defecto en Proxmox): la página puesta a cero vuelve con el contenido del
//     dorado;
//   - los que dicen que todo son datos (NFSv3, virtiofs, FUSE sin lseek): los
//     huecos se aplican como ceros y la memoria queda casi entera a cero.
//
// huecosFiables lo prueba una vez por directorio con un fichero de cuatro
// páginas (datos, ceros escritos, hueco, hueco) y lo recuerda. Si no cuadra,
// las copias se congelan enteras.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var huecosProbados sync.Map // directorio -> bool

// huecosFiables dice si en dir un hueco y una página de ceros escrita se ven
// distintos con SEEK_DATA/SEEK_HOLE. Ante la duda, no.
func huecosFiables(dir string) bool {
	if v, ok := huecosProbados.Load(dir); ok {
		return v.(bool)
	}
	ok := probarHuecos(dir)
	huecosProbados.Store(dir, ok)
	return ok
}

func probarHuecos(dir string) bool {
	const pag = 4096
	var sufijo [8]byte
	_, _ = rand.Read(sufijo[:])
	ruta := filepath.Join(dir, ".kling-huecos-"+hex.EncodeToString(sufijo[:]))
	f, err := os.OpenFile(ruta, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false
	}
	defer os.Remove(ruta)
	defer f.Close()
	datos := make([]byte, 2*pag)
	for i := 0; i < pag; i++ {
		datos[i] = 0xab
	}
	if _, err := f.WriteAt(datos, 0); err != nil {
		return false
	}
	if err := f.Truncate(4 * pag); err != nil {
		return false
	}
	if err := f.Sync(); err != nil {
		return false
	}
	// Las dos primeras páginas son datos (la de ceros también: se escribió).
	if d, err := f.Seek(0, seekData); err != nil || d != 0 {
		return false
	}
	if h, err := f.Seek(0, seekHole); err != nil || h < 2*pag {
		return false
	}
	// Y lo que nunca se escribió es hueco hasta el final.
	_, err = f.Seek(2*pag, seekData)
	return errors.Is(err, syscall.ENXIO)
}

// siguientesDatosEstricto es siguientesDatos para aplicar un diferencial: un
// error de SEEK_DATA que no sea "no hay más datos" no se toma como "todo es
// datos" (eso aplicaría los huecos como ceros), se devuelve.
func siguientesDatosEstricto(f *os.File, off, tam int64) (ini, fin int64, err error) {
	d, err := f.Seek(off, seekData)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return tam, tam, nil
		}
		return 0, 0, err
	}
	if d >= tam {
		return tam, tam, nil
	}
	d &^= bloqueDisperso - 1
	h, err := f.Seek(d, seekHole)
	if err != nil {
		return 0, 0, err
	}
	if h > tam || h <= d {
		h = tam
	}
	return d, h, nil
}
