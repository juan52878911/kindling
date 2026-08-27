// Package assets lleva dentro del binario los artefactos que una microVM no
// puede fabricar sola: el kernel invitado y una imagen base mínima.
//
// Existe para que instalar kindling sea un binario y no tres scripts. Hasta
// ahora, una instalación desde cero exigía correr a mano 30-fetch-artifacts.sh
// (descarga el vmlinux del CI de Firecracker) y 70-build-minimal-image.sh
// (construye el ext4 de Alpine), y ninguno de los dos se puede ejecutar desde
// el CLI: el segundo monta un loopback y hace chroot.
//
// El embebido es OPCIONAL y va detrás de la etiqueta de compilación
// `embed_assets` por dos razones, y las dos son necesarias:
//
//   - go:embed exige que los ficheros EXISTAN al compilar. Sin la etiqueta, un
//     `go build ./...` recién clonado el repo fallaría en cualquier máquina que
//     no haya descargado antes 20 MB de kernel.
//   - El CLI que se instala en un Mac no arranca microVMs jamás. Cargarlo con
//     un kernel de Linux que nunca va a usar es peso muerto en cada descarga.
//
// Así que el binario del daemon se compila con `make daemon-full` y lleva los
// artefactos; el del CLI se compila normal y no lleva nada. Ambos usan esta
// misma API: Embedded() dice cuál es cuál.
package assets

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Artifacts son los ficheros que el daemon espera en $KLING_ROOT/images.
//
// Los nombres NO son decorativos: manager.KernelPath() busca exactamente
// "vmlinux" ahí, y `kling run -image min` resuelve a "min.ext4". Cambiar uno
// aquí sin cambiarlo allí deja una instalación que parece completa y no
// arranca.
var Artifacts = []string{"vmlinux", "min.ext4"}

// Embedded dice si ESTE binario lleva los artefactos dentro.
//
// Sirve para que quien llame distinga "no había nada que copiar" de "lo copié
// todo", que desde fuera se parecen y significan cosas opuestas.
func Embedded() bool {
	_, ok := source()
	return ok
}

// Materialize escribe en dir los artefactos que FALTEN, y devuelve las rutas
// que ha creado.
//
// Dos decisiones que no son evidentes:
//
//   - No pisa lo que ya esté. Una imagen que su dueño construyó —con node, con
//     python, con lo que sea que necesite su servidor MCP— vale infinitamente
//     más que la de fábrica, y `kling up` se ejecuta más de una vez. La regla
//     es: solo rellena huecos.
//   - Escribe a .tmp y renombra. Un Ctrl-C a mitad de copiar 20 MB no puede
//     dejar un fichero con el nombre bueno y la mitad del contenido: eso no da
//     un error, da un kernel que no arranca y una tarde de depuración.
//
// Sin la etiqueta `embed_assets` no hay nada dentro, y eso no es un fallo: se
// devuelve una lista vacía y ningún error. Quien quiera avisar de que este
// binario venía pelado que consulte Embedded() antes.
func Materialize(dir string) ([]string, error) {
	src, ok := source()
	if !ok {
		return nil, nil
	}
	return copiarDesde(src, dir)
}

// copiarDesde es Materialize con el origen inyectado, para poder probarlo sin
// depender de que este binario se haya compilado con `embed_assets`.
func copiarDesde(src fs.FS, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	var written []string
	for _, name := range Artifacts {
		dst := filepath.Join(dir, name)
		if _, err := os.Stat(dst); err == nil {
			continue // ya está: es de su dueño, no se toca
		} else if !errors.Is(err, os.ErrNotExist) {
			return written, err
		}
		if err := copyOut(src, name, dst); err != nil {
			return written, fmt.Errorf("%s: %w", dst, err)
		}
		written = append(written, dst)
	}
	return written, nil
}

// copyOut vuelca un artefacto a su destino final de forma atómica.
func copyOut(src fs.FS, name, dst string) error {
	in, err := src.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()

	// El .tmp va en el MISMO directorio que el destino a propósito: rename solo
	// es atómico dentro del mismo sistema de ficheros, y /tmp suele ser otro.
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // si todo va bien ya no existe y esto no hace nada

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// Sync antes del rename: sin él, un corte de corriente puede dejar el
	// nombre definitivo apuntando a datos que nunca llegaron al disco.
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
