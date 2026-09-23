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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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
}

// volcadoEnCurso deja la marca de que empieza un volcado y retira el sello del
// anterior: a partir de aquí, lo que hubiera en disco deja de valer.
func volcadoEnCurso(dir string) error {
	_ = os.Remove(filepath.Join(dir, marcaOK))
	return durable.Escribir(filepath.Join(dir, marcaEnCurso), []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// sellarVolcado escribe el sello cuando snap.file y mem.file ya están completos
// y en su sitio, y retira la marca de volcado en curso.
func sellarVolcado(dir string) error {
	snapSHA, err := sha256Fichero(filepath.Join(dir, "snap.file"))
	if err != nil {
		return err
	}
	fi, err := os.Stat(filepath.Join(dir, "mem.file"))
	if err != nil {
		return err
	}
	b, _ := json.Marshal(sello{SnapSHA256: snapSHA, MemBytes: fi.Size(), At: time.Now()})
	if err := durable.Escribir(filepath.Join(dir, marcaOK), b, 0o600); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, marcaEnCurso))
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
	got, err := sha256Fichero(filepath.Join(dir, "snap.file"))
	if err != nil {
		return err
	}
	if got != s.SnapSHA256 {
		return fmt.Errorf("%w: the state file changed after it was frozen", errVolcadoIncompleto)
	}
	return nil
}

func sha256Fichero(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
