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
//   - ninguna de las dos: una máquina congelada antes de que esto existiera. Se
//     acepta, que es lo que se hacía siempre; romperlas obligaría a recrearlas.
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
	"time"

	"github.com/juan52878911/kindling/pkg/digest"
	"github.com/juan52878911/kindling/pkg/durable"
)

const (
	marcaEnCurso = "volcado.en-curso"
	marcaOK      = "volcado.ok"
)

type sello struct {
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
func sellarVolcado(dir, kernelSHA, vmm, diffBase string) error {
	snapSHA, err := digest.File(filepath.Join(dir, "snap.file"))
	if err != nil {
		return err
	}
	fi, err := os.Stat(filepath.Join(dir, "mem.file"))
	if err != nil {
		return err
	}
	b, _ := json.Marshal(sello{SnapSHA256: snapSHA, MemBytes: fi.Size(), At: time.Now(), KernelSHA256: kernelSHA, VMM: vmm, DiffBase: diffBase})
	if err := durable.Escribir(filepath.Join(dir, marcaOK), b, 0o600); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, marcaEnCurso))
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

// errVolcadoIncompleto es que el volcado no se puede usar.
var errVolcadoIncompleto = errors.New("the frozen state is incomplete")

// volcadoValido dice si el volcado de dir se puede restaurar.
func volcadoValido(dir string) error {
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
		return nil // anterior a los sellos
	}
	if err != nil {
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
