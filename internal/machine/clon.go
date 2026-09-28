package machine

// Clonar discos: la primitiva que hace baratas las ramas de un volumen.
//
// Un clon (reflink en Linux, clonefile en macOS) no copia datos: el destino
// comparte los bloques del origen y solo paga lo que cada uno escribe después.
// Cuesta milisegundos con 1 MiB o con 100 GiB. Una copia, en cambio, cuesta
// tiempo y disco proporcionales al tamaño, y por eso NUNCA se hace sin que se
// pida: un `volume clone` que tarda un segundo en XFS y veinte minutos en ext4
// no puede tener la misma cara.
//
// SISTEMAS DE FICHEROS. No se elige ninguno ni se asume ninguno: se prueba. El
// daemon no sabe de antemano si <root> está en XFS, btrfs, ZFS o ext4, ni si
// volumes/, machines/ y snapshots/ viven en el mismo disco (un bind mount o un
// disco aparte para los volúmenes son configuraciones normales), ni si ese XFS
// se formateó con reflink. Todo eso cambia sin reiniciar el daemon: basta un
// mount. Así que la sonda clona de verdad un fichero de prueba desde volumes/
// a cada directorio y apunta qué pasó, y cada clon real vuelve a intentarlo
// sin fiarse de la sonda: ella es para informar, no para decidir.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// errSinClon: el sistema de ficheros (o la combinación origen/destino) no
// sabe clonar. No es un fallo del host: es "aquí solo cabe copiar".
var errSinClon = errors.New("the host filesystem cannot clone files")

// ErrSinClon es errSinClon para quien está fuera del paquete (el daemon
// contesta 409 con él).
var ErrSinClon = errSinClon

// ErrVolumenEnUso: el volumen lo tiene alguien y la operación no puede
// esperar a que lo suelte. El daemon contesta 409 con él.
var ErrVolumenEnUso = errors.New("volume in use")

// clonar es clonarFichero, sustituible en los tests: en un ext4 no hay clon
// posible y hay caminos que solo se ejercitan si lo hay.
var clonar = clonarFichero

// copiar es copiarDisco sin el ctx en la firma, sustituible igual.
var copiar = func(ctx context.Context, src, dst string) error {
	if out, err := copiarDisco(ctx, src, dst); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dirsSonda son los directorios bajo <root> que la sonda prueba como destino
// de un clon desde volumes/. volumes/ consigo mismo es `volume clone`; el resto
// es lo que necesitará un fork que clone volúmenes: la copia de cada rama vive
// en machines/<id>/, la del snapshot en snapshots/<n>/, y jailed en jails/.
var dirsSonda = []string{"volumes", "machines", "snapshots", "jails"}

// sondaVigencia es cuánto vale una sonda. Poco: un mount cambia la respuesta
// sin que el daemon se entere, y la sonda cuesta un par de ficheros de 64 KiB.
const sondaVigencia = time.Minute

// sondaClon guarda la última sonda. Va fuera de Manager.mu a propósito: sondar
// hace E/S y no tiene nada que ver con las máquinas.
type sondaClon struct {
	mu    sync.Mutex
	info  *api.CloneInfo
	hecha time.Time
}

var sondas sync.Map // root → *sondaClon

// CloneInfo devuelve la sonda de clonado de este daemon, rehaciéndola si
// caducó.
func (m *Manager) CloneInfo() *api.CloneInfo {
	v, _ := sondas.LoadOrStore(m.root, &sondaClon{})
	s := v.(*sondaClon)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil || time.Since(s.hecha) > sondaVigencia {
		s.info = sondar(m.root)
		s.hecha = time.Now()
	}
	return s.info
}

// sondar clona un fichero de prueba de volumes/ a cada directorio de
// dirsSonda que exista. Los ficheros empiezan por punto: los barridos de
// huérfanos (sweepMachineDirs, barrerForks) y Volumes() los ignoran, así que
// uno que se quedase a medias por un corte no confunde a nadie.
func sondar(root string) *api.CloneInfo {
	info := &api.CloneInfo{}
	vols := filepath.Join(root, "volumes")
	if err := os.MkdirAll(vols, 0o755); err != nil {
		return info
	}
	marca := ".kling-clone-probe-" + strconv.Itoa(os.Getpid())
	src := filepath.Join(vols, marca)
	_ = os.Remove(src)
	// Datos de verdad, no ceros ni un fichero vacío: un fichero sin extents
	// se "clona" en algunos sistemas sin que el clon se llegue a probar.
	datos := make([]byte, 64<<10)
	for i := range datos {
		datos[i] = byte(i*7 + 1)
	}
	if err := os.WriteFile(src, datos, 0o600); err != nil {
		return info
	}
	defer os.Remove(src)

	for _, d := range dirsSonda {
		dir := filepath.Join(root, d)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue // no se crea nada solo para sondarlo
		}
		r := api.CloneDir{Dir: d, Filesystem: tipoFS(dir)}
		dst := filepath.Join(dir, marca+"-dst")
		_ = os.Remove(dst)
		if err := clonar(src, dst); err != nil {
			// Solo el motivo ("operation not supported"): la línea de
			// `kling status -v` ya dice que es un clon lo que falló.
			r.Error = strings.TrimPrefix(err.Error(), errSinClon.Error()+": ")
		} else {
			r.Reflink = true
		}
		_ = os.Remove(dst)
		if d == "volumes" && r.Reflink {
			info.Method = metodoClon
		}
		info.Dirs = append(info.Dirs, r)
	}
	return info
}

// CloneVolume crea el volumen dst como clon de src.
//
// Con reflink es instantáneo y dst no ocupa nada hasta que alguien escribe en
// uno de los dos. Sin reflink devuelve errSinClon, salvo que permitirCopia: en
// ese caso copia (disperso), que tarda lo que tarde.
//
// src NO puede tener un escritor: clonar un ext4 montado en escritura por una
// microVM viva da una foto a medio escribir —la caché del invitado no ha
// llegado al fichero— y un journal que no casa con los datos. Los lectores sí
// valen: si nadie escribe, los bloques no cambian. Para clonar en caliente un
// volumen en uso hace falta pausar la máquina y clonar con la memoria, que es
// el fork con volúmenes y no esto.
//
// Mientras dura el clon, src queda reservado como leído (nadie puede montarlo
// en escritura ni borrarlo) y dst reservado como escrito (nadie puede clonar
// otro encima). Con reflink la reserva dura milisegundos; con copia, lo que
// dure la copia, y por eso la copia va fuera del cerrojo.
func (m *Manager) CloneVolume(ctx context.Context, src, dst string, permitirCopia bool) (*api.CloneVolumeResult, error) {
	for _, n := range []string{src, dst} {
		if !reVolume.MatchString(n) {
			return nil, fmt.Errorf("invalid volume name %q: lowercase letters, digits, hyphen and underscore", n)
		}
	}
	if src == dst {
		return nil, fmt.Errorf("source and destination are the same volume")
	}
	srcPath, dstPath := m.volumePath(src), m.volumePath(dst)

	quien := "clone:" + dst
	m.mu.Lock()
	if _, err := os.Stat(srcPath); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("volume %q not found", src)
	}
	if _, err := os.Stat(dstPath); err == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("volume %q: %w", dst, os.ErrExist)
	}
	uso := m.volumeUsersLocked()
	if w := uso[src].writers; len(w) > 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %q is mounted read-write by %s, and a clone taken now would catch it "+
			"half-written. Stop that machine first (kling stop %s), or mount it read-only",
			ErrVolumenEnUso, src, strings.Join(w, ", "), strings.Fields(w[0])[0])
	}
	if u := uso[dst].all(); len(u) > 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %q is being created right now (%s)", ErrVolumenEnUso, dst, strings.Join(u, ", "))
	}
	if m.volReservas == nil {
		m.volReservas = map[string][]reservaVolumen{}
	}
	m.volReservas[src] = append(m.volReservas[src], reservaVolumen{maquina: quien, nombre: "clone → " + dst, soloLect: true})
	m.volReservas[dst] = append(m.volReservas[dst], reservaVolumen{maquina: quien, nombre: "clone ← " + src})
	m.mu.Unlock()
	defer m.soltarReservas(quien)

	// Como en CreateVolume, se construye aparte y se publica al final, para
	// que existir implique estar completo y en disco. Pero el temporal NO es
	// <dst>.ext4.tmp: ese es el de CreateVolume, que lo borra al empezar, y un
	// `volume create` del mismo nombre a mitad de una copia larga se llevaría
	// la copia por delante. Empieza por punto para que Volumes() lo ignore.
	tmp := filepath.Join(m.volumesDir(), "."+dst+".clone-"+strconv.Itoa(os.Getpid())+".tmp")
	_ = os.Remove(tmp)
	res := &api.CloneVolumeResult{Filesystem: tipoFS(m.volumesDir())}
	t0 := time.Now()
	err := clonar(srcPath, tmp)
	switch {
	case err == nil:
		res.Method = metodoClon
	case errors.Is(err, errSinClon) && permitirCopia:
		_ = os.Remove(tmp)
		if err := copiar(ctx, srcPath, tmp); err != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("copying volume: %w", err)
		}
		res.Method = api.CloneCopy
	case errors.Is(err, errSinClon):
		fs := res.Filesystem
		if fs == "" {
			fs = "its filesystem"
		}
		return nil, fmt.Errorf("%w (%s on %s). Put %s on XFS (mkfs.xfs, reflink is the default) or btrfs "+
			"to clone in milliseconds, or allow a full copy (kling volume clone -copy)",
			err, fs, m.volumesDir(), m.root)
	default:
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("cloning volume: %w", err)
	}
	if err := publicarSinPisar(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	res.ElapsedMS = time.Since(t0).Milliseconds()
	// Igual que un volumen recién creado: el VMM corre sin privilegios y tiene
	// que poder escribir en él.
	if m.priv != nil && m.priv.UID > 0 {
		_ = os.Chown(dstPath, m.priv.UID, -1)
	}
	v, err := m.statVolume(dst)
	if err != nil {
		return nil, err
	}
	res.Volume = v
	return res, nil
}

// publicarSinPisar es durable.Renombrar pero sin reemplazar nunca un destino
// que ya exista: fsync del fichero, link(2) —que falla con EEXIST en vez de
// pisar—, unlink del temporal y fsync del directorio. Un rename habría
// sustituido en silencio un volumen creado con el mismo nombre mientras se
// copiaba este.
func publicarSinPisar(tmp, destino string) error {
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp, destino); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("volume %q appeared while cloning: %w",
				strings.TrimSuffix(filepath.Base(destino), ".ext4"), os.ErrExist)
		}
		return err
	}
	_ = os.Remove(tmp)
	if d, err := os.Open(filepath.Dir(destino)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
