package credproxy

// Cuerpo de la petición saliente hacia el proveedor: dónde se sustituye el
// marcador y con qué Content-Length se reenvía.
//
// POR QUÉ NO SIEMPRE CHUNKED: sustituir el marcador cambia la longitud (el
// marcador y la clave no miden lo mismo), así que la longitud final no se
// sabe hasta terminar. Reenviar chunked es lo más simple, pero algunos
// proveedores de API no aceptan una subida chunked y la rechazan. Si el
// invitado SÍ declaró Content-Length —o sea, no llegó chunked—, se puede
// conocer la longitud final sin adivinarla: basta con sustituir primero y
// contar. La única forma de hacerlo sin subir la memoria por petición es
// derramar lo sustituido a un fichero temporal en vez de a un buffer.
//
// EL RIESGO de ese fichero: lleva la clave real en claro mientras dura la
// petición. Se minimiza así:
//   - Nombre aleatorio (os.CreateTemp) e ilegible para quien no sea el
//     propietario del proceso: permisos 0600 explícitos, no solo los que
//     ponga el sistema por defecto.
//   - Se borra en cuanto se deja de necesitar, tanto si el proveedor
//     contesta como si falla la petición o el proceso aborta a mitad
//     (cerrarCuerpo va en un defer del llamador, así que corre también si
//     ServeHTTP hace panic(http.ErrAbortHandler)).
//   - El directorio lo elige el llamador (TempDir de Options): el daemon
//     puede pasar uno bajo $KLING_ROOT, que solo lee él. Sin TempDir, cae en
//     os.TempDir(): normalmente compartido por todo el host, pero un archivo
//     0600 con nombre aleatorio ahí no es legible por otro usuario sin ser
//     root, así que sigue siendo razonable como valor por defecto. Si el
//     llamador tiene un directorio propio de verdad (el caso del daemon),
//     conviene pasarlo: un proceso hostil en la MISMA cuenta del daemon (que
//     aquí no se protege de todos modos: ver el aviso de package credproxy)
//     tendría que acertar el nombre para leerlo, y con un directorio ajeno al
//     de /tmp ni siquiera puede listarlo.
//
// El cuerpo del invitado sigue acotado a MaxBody y pasando por el vigía de
// plazos igual que antes de este fichero: aquí solo cambia adónde va lo ya
// sustituido cuando no cabe en memoria.

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"slices"
)

// cerrarCuerpo libera lo que cuerpoSaliente reservó para el cuerpo (hoy, solo
// el fichero temporal cuando lo hay). nil si no hay nada que liberar. El
// llamador lo pone en un defer justo después de comprobar el error, para que
// corra pase lo que pase con la petición al proveedor.
type cerrarCuerpo func()

// cuerpoSaliente prepara el cuerpo que va al proveedor: el del invitado
// (acotado a MaxBody y vigilado) con el marcador cambiado por la clave en
// flujo. Lee por adelantado hasta MaxSwapBody de salida:
//   - Si el cuerpo termina antes, sale entero con su Content-Length (un
//     formulario OAuth, un JSON normal).
//   - Si no cabe y el invitado NO declaró Content-Length (llegó chunked), lo
//     leído va delante del resto y sale chunked (longitud -1), como siempre:
//     no hay una longitud que prometer de todos modos.
//   - Si no cabe pero el invitado SÍ declaró Content-Length, se derrama el
//     resto a un fichero temporal (ver el aviso arriba) y se reenvía con la
//     longitud exacta del fichero: evita el chunked que algunos proveedores
//     rechazan, sin retener el cuerpo entero en memoria.
//
// tempDir es dónde crear ese fichero; "" usa os.TempDir().
func cuerpoSaliente(r *http.Request, w http.ResponseWriter, cs []Credential, v *vigia, pl plazosInvitado, tempDir string) (io.Reader, int64, cerrarCuerpo, error) {
	if r.ContentLength == 0 {
		return nil, 0, nil, nil
	}
	s := nuevoSustituidor(lectorVigilado{r: http.MaxBytesReader(w, r.Body, MaxBody), v: v, p: pl}, cs)
	// A mano y no con io.ReadAll: su crecimiento al doble reservaría hasta 2 MiB
	// para retener 1. Aquí la capacidad no pasa de MaxSwapBody+1.
	const tope = MaxSwapBody + 1
	hint := 32 << 10
	if r.ContentLength > 0 {
		hint = int(min(r.ContentLength+1024, tope))
	}
	buf := make([]byte, 0, hint)
	for len(buf) < tope {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(cap(buf), tope-len(buf)))
		}
		n, err := s.Read(buf[len(buf):min(cap(buf), tope)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return bytes.NewReader(buf), int64(len(buf)), nil, nil
		}
		if err != nil {
			return nil, 0, nil, err
		}
	}
	if r.ContentLength < 0 {
		// Chunked desde el invitado: sin Content-Length que prometer, sigue
		// saliendo chunked como antes de este fichero.
		return io.MultiReader(bytes.NewReader(buf), s), -1, nil, nil
	}
	return cuerpoAFichero(buf, s, tempDir)
}

// cuerpoAFichero derrama buf (lo ya sustituido) y el resto de s a un fichero
// temporal privado, y devuelve el fichero listo para leer desde el principio
// con su tamaño exacto. Lo borra y lo cierra él mismo si algo falla; si todo
// va bien, esa limpieza queda en el cerrarCuerpo que devuelve, para que corra
// el llamador cuando termine la petición al proveedor (con éxito o sin él).
func cuerpoAFichero(buf []byte, resto io.Reader, tempDir string) (io.Reader, int64, cerrarCuerpo, error) {
	f, err := os.CreateTemp(tempDir, "kindling-credproxy-*.tmp")
	if err != nil {
		return nil, 0, nil, err
	}
	limpiar := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	// Explícito y no solo confiar en el 0600 por defecto de CreateTemp: que
	// quede claro en el propio código, no solo en la documentación de arriba,
	// que este fichero lleva una clave y no se abre a nadie más.
	if err := f.Chmod(0o600); err != nil {
		limpiar()
		return nil, 0, nil, err
	}
	if _, err := f.Write(buf); err != nil {
		limpiar()
		return nil, 0, nil, err
	}
	tam, err := io.Copy(f, resto)
	if err != nil {
		limpiar()
		return nil, 0, nil, err
	}
	tam += int64(len(buf))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		limpiar()
		return nil, 0, nil, err
	}
	return f, tam, limpiar, nil
}
