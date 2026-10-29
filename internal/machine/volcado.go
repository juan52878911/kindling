package machine

// El sello de un volcado completo.
//
// Firecracker escribe snap.file y mem.file directamente, sin fichero temporal ni
// rename. Si el daemon muere a mitad de una congelación —un corte, el OOM
// killer— quedan dos ficheros que EXISTEN pero están a medias, y lo único que
// miraba reconcile era que existieran: marcaba la máquina warm, y el siguiente
// thaw cargaba un volcado truncado con un error que no señalaba a ninguna parte.
//
// La solución son dos marcas. Antes de volcar se deja volcado.en-curso; al
// terminar, cuando los ficheros ya están en su sitio, se escribe volcado.ok con
// el sha256 del estado y el tamaño de la memoria, y se retira la primera.
//
//   - en-curso sin ok: el volcado se interrumpió. No vale.
//   - ok: vale si el estado y el tamaño cuadran.
//   - ninguna de las dos: una máquina congelada antes de que esto existiera
//     (v0.8.0). Se acepta, que es lo que se hacía siempre; romperlas obligaría
//     a recrearlas. Pero solo si su FrozenAt es de antes: una congelada después
//     siempre deja una de las dos, y que falten las dos es que el disco perdió
//     algo (un corte que se llevó la marca en curso): no vale.
//
// El sha256 es del snap.file y no del mem.file por lo mismo que en los dorados:
// el estado pesa KiB y la memoria GiB, y hashear la memoria en cada thaw mataría
// los milisegundos que son la razón de ser del proyecto. Para la memoria basta
// el tamaño: un volcado truncado es, por definición, más corto.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/digest"
	"github.com/juan52878911/kindling/pkg/durable"
	"github.com/juan52878911/kindling/pkg/esquema"
)

const (
	marcaEnCurso = "volcado.en-curso"
	marcaOK      = "volcado.ok"
)

// selloDesde es cuándo salió la primera versión que deja las marcas (v0.8.0,
// 308223b9): un volcado de después sin ninguna de las dos no se da por bueno.
var selloDesde = time.Date(2026, 9, 23, 5, 28, 42, 0, time.UTC)

// versionSello es la versión de volcado.ok (ver pkg/esquema).
//
//	0: sin campo (v0.8-v0.20). Con los opcionales vmm y diff_base (v0.18) y
//	   sin_agente (v0.19), que un kling anterior ignora sin daño salvo
//	   diff_base, al que protege que state.json pase a 2 (manager.go).
//	1: lo mismo, con "schema": 1.
//
// Súbela cuando un kling anterior fuera a dar por bueno un volcado que no
// sabría cargar: uno más nuevo no se descongela (se dice que hay que
// actualizar kling) y no se toca.
const versionSello = 1

type sello struct {
	esquema.Cabecera
	SnapSHA256 string    `json:"snap_sha256"`
	MemBytes   int64     `json:"mem_bytes"`
	At         time.Time `json:"at"`
	// KernelSHA256 es el vmlinux instalado al congelar (K2, como el de los
	// dorados en meta.json). Opcional: los sellos anteriores no lo llevan y se
	// descongelan igual (ver kernelDelVolcado).
	KernelSHA256 string `json:"kernel_sha256,omitempty"`
	// VMM es con qué VMM y versión se volcó ("firecracker 1.12.0"), como el
	// de los dorados (meta.go). Opcional: sin él no se compara.
	VMM string `json:"vmm,omitempty"`
	// DiffBase: el mem.file es un diferencial (solo las páginas escritas
	// desde el dorado) y este es el mem.file del dorado sobre el que va
	// (diff_volcado.go). Vacío: la RAM entera, como siempre.
	DiffBase string `json:"diff_base,omitempty"`
	// SinAgente: nadie escuchaba en el puerto del agente al congelar; el thaw
	// no resincroniza (mismo criterio que Snapshot.SinAgente) y sobrevive a un
	// reinicio del daemon.
	SinAgente bool `json:"sin_agente,omitempty"`
}

// volcadoEnCurso deja la marca de que empieza un volcado y retira el sello del
// anterior: a partir de aquí, lo que hubiera en disco deja de valer.
func volcadoEnCurso(dir string) error {
	_ = os.Remove(filepath.Join(dir, marcaOK))
	return durable.Escribir(filepath.Join(dir, marcaEnCurso), []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// sellarVolcado escribe el sello cuando snap.file y mem.file ya están completos
// y en su sitio, y retira la marca de volcado en curso. kernelSHA es el
// sha256 del vmlinux instalado y vmm el VMM que volcó; vacíos, el sello no
// los lleva. diffBase, si el mem.file es un diferencial, es su base
// (diff_volcado.go).
func sellarVolcado(dir, kernelSHA, vmm, diffBase string, sinAgente bool) error {
	snapSHA, err := digest.File(filepath.Join(dir, "snap.file"))
	if err != nil {
		return err
	}
	fi, err := os.Stat(filepath.Join(dir, "mem.file"))
	if err != nil {
		return err
	}
	b, _ := json.Marshal(sello{Cabecera: esquema.Cabecera{Schema: versionSello}, SnapSHA256: snapSHA, MemBytes: fi.Size(),
		At: time.Now(), KernelSHA256: kernelSHA, VMM: vmm, DiffBase: diffBase, SinAgente: sinAgente})
	if err := durable.Escribir(filepath.Join(dir, marcaOK), b, 0o600); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, marcaEnCurso))
}

// sincronizarVolcado lleva a disco snap.file, mem.file y el overlay antes del
// sello o del meta. Éstos son durables (durable.Escribir), pero los ficheros
// del volcado los escribió el VMM y seguían en la caché de página: tras un
// corte de luz quedaba un sello que daba por bueno un mem.file con huecos o
// un overlay a medias. Va con la máquina ya muerta o reanudada, nunca en la
// pausa. Los que no existen se saltan.
//
// Con barrera (durable.Ordenar) y no con un F_FULLFSYNC por fichero: lo
// que importa es que lleguen al disco antes que el sello o el meta, y el
// F_FULLFSYNC con el que acaba ese durable.Escribir los asienta a todos. En
// macOS son ~5 ms por F_FULLFSYNC; una por congelación, no cuatro.
//
// snap y mem son del daemon (recuperados de la jaula con comprobación), y
// mem.file puede ser un enlace suyo al almacén (diff_volcado.go): se siguen.
// El overlay puede estar en un directorio del VMM: sin seguir enlaces (un
// enlace se salta, el overlay de verdad lo da overlayParaLeer) y solo un
// fichero regular.
func sincronizarVolcado(snap, mem, overlay string) error {
	if err := ordenarFichero(snap, 0, durable.Ordenar); err != nil {
		return err
	}
	if err := ordenarFichero(mem, 0, durable.Ordenar); err != nil {
		return err
	}
	err := ordenarFichero(overlay, syscall.O_NOFOLLOW, durable.Ordenar)
	if errors.Is(err, syscall.ELOOP) {
		return nil
	}
	return err
}

// sincronizarFichero asienta ruta en el disco físico por sí solo (File.Sync:
// F_FULLFSYNC en macOS), para cuando no le sigue ninguna escritura durable.
func sincronizarFichero(ruta string, flag int) error {
	return ordenarFichero(ruta, flag, (*os.File).Sync)
}

func ordenarFichero(ruta string, flag int, sync func(*os.File) error) error {
	if ruta == "" {
		return nil
	}
	f, err := os.OpenFile(ruta, os.O_RDONLY|syscall.O_NONBLOCK|flag, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return err
	}
	if err := sync(f); err != nil {
		return fmt.Errorf("fsync %s: %w", filepath.Base(ruta), err)
	}
	return nil
}

// borrarVolcadoParcial retira lo que dejó un volcado que falló a medias: el
// snap.file y el mem.file que Firecracker llegó a escribir (en la raíz de su
// chroot si está enjaulada, en dir si no) y la marca de volcado en curso. La
// máquina sigue running, así que nada de esto vale ni lo recogería nadie:
// reconcile y el GC solo miran las warm, y un mem.file del tamaño de la RAM se
// quedaba ahí hasta el rm. Lo que no se pueda borrar se avisa y no bloquea.
func (m *Manager) borrarVolcadoParcial(id string, jailed bool, dir string) {
	for _, f := range []string{"snap.file", "mem.file", memDiff} {
		var err error
		if jailed {
			err = borrarEnJail(m.jailRoot(id), "/"+f)
		} else if err = os.Remove(filepath.Join(dir, f)); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			log.Printf("warning: %s: could not remove the partial %s of a failed freeze: %v", id[:12], f, err)
		}
	}
	_ = os.Remove(filepath.Join(dir, marcaEnCurso))
}

// borrarVolcado retira el volcado entero de la máquina id —estado, memoria,
// diferencial, la memoria reconstruida de un thaw, sus marcas y su espejo en
// el almacén— cuando ya no lo va a cargar nadie (Stop). Lo que no se pueda
// borrar se avisa y no bloquea.
func (m *Manager) borrarVolcado(id string) {
	dir := m.dir(id)
	for _, f := range []string{"snap.file", "mem.file", memDiff, memFull, marcaOK, marcaEnCurso} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("warning: %s: could not remove its %s: %v", shortID(id), f, err)
		}
	}
	m.borrarMemoriaAlmacen(id)
}

// errVolcadoIncompleto es que el volcado no se puede usar.
var errVolcadoIncompleto = errors.New("the frozen state is incomplete")

// volcadoValido dice si el volcado de dir se puede restaurar. congelada es
// el FrozenAt de la máquina (nil si no consta): de después de selloDesde, el
// volcado tiene que llevar sello.
func volcadoValido(dir string, congelada *time.Time) error {
	for _, f := range []string{"snap.file", "mem.file"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return fmt.Errorf("%w: %s is missing", errVolcadoIncompleto, f)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, marcaOK))
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(filepath.Join(dir, marcaEnCurso)); err == nil {
			return fmt.Errorf("%w: the freeze was interrupted before it finished writing", errVolcadoIncompleto)
		}
		if congelada != nil && congelada.After(selloDesde) {
			return fmt.Errorf("%w: it was frozen on %s by a version that seals its freezes, and the seal is missing",
				errVolcadoIncompleto, congelada.Format(time.RFC3339))
		}
		return nil // anterior a los sellos
	}
	if err != nil {
		return err
	}
	// De un kling más nuevo: no está incompleto, y no se sabe cargar. Es un
	// *esquema.ErrMasNuevo, no errVolcadoIncompleto, para que nadie lo trate
	// como basura (gc.go lo conserva).
	if _, err := esquema.Comprobar(filepath.Join(dir, marcaOK), raw, versionSello); esquema.EsMasNuevo(err) {
		return err
	}
	var s sello
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%w: unreadable seal: %v", errVolcadoIncompleto, err)
	}

	fi, err := os.Stat(filepath.Join(dir, "mem.file"))
	if err != nil {
		return err
	}
	if fi.Size() != s.MemBytes {
		return fmt.Errorf("%w: the memory dump is %d bytes and should be %d", errVolcadoIncompleto, fi.Size(), s.MemBytes)
	}
	got, err := digest.File(filepath.Join(dir, "snap.file"))
	if err != nil {
		return err
	}
	if got != s.SnapSHA256 {
		return fmt.Errorf("%w: the state file changed after it was frozen", errVolcadoIncompleto)
	}
	return nil
}

// kernelDelVolcado es el kernel_sha256 del sello de dir, o "" si el sello no
// existe, no se lee o es anterior al campo: en todos esos casos no hay nada
// con qué comparar y el thaw sigue como siempre. Se llama tras volcadoValido,
// que ya rechazó los sellos ilegibles.
func kernelDelVolcado(dir string) string { return leerSello(dir).KernelSHA256 }

// vmmDelVolcado es el VMM grabado en el sello de dir, o "" si no consta.
func vmmDelVolcado(dir string) string { return leerSello(dir).VMM }

// leerSello lee el sello de dir; vacío si no hay o no se lee.
func leerSello(dir string) sello {
	var s sello
	raw, err := os.ReadFile(filepath.Join(dir, marcaOK))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

// plazoVolcado es cuánto se le deja a Firecracker para volcar (Snapshot) o
// cargar (LoadSnapshot) la memoria de una microVM de memMiB: 10 s fijos más
// 20 s por GiB, y nunca menos de los 30 s que ya tenía toda petición.
//
// El tope plano de 30 s del cliente (F-01) cortaba un volcado de 4 GiB sobre
// un disco lento. Cortar no para a Firecracker: el Freeze se daba por fallido
// y reanudaba mientras el VMM seguía escribiendo un mem.file que nadie iba a
// usar, con la marca de volcado en curso puesta hasta el siguiente Remove.
// 20 s/GiB son ~50 MiB/s, un disco malo; un volcado que no cabe en eso no es
// lento, está colgado, y ahí sí conviene rendirse.
//
// memMiB es el tamaño de la VM tal y como la ve Firecracker: con techo de
// resize es MemMaxMiB (ver boot), no MemMiB. Quien llama pasa el mayor.
func plazoVolcado(memMiB int) time.Duration {
	return max(30*time.Second, 10*time.Second+time.Duration(memMiB)*20*time.Second/1024)
}
