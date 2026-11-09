package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// Pistas: la segunda línea de un error.
//
// Un "error: machine \"x\" does not exist" a secas obliga a quien lo lee a
// saber qué comando lista las máquinas. La pista es ese comando. Se decide por
// el código estable del error (api.ErrorCode, hintsPorCodigo) cuando el daemon
// lo manda, y si no por el TEXTO: los tipos se pierden por HTTP, y un daemon
// anterior a "error-codes" solo manda el mensaje.

// hintsPorCodigo son las pistas de los códigos del daemon que tienen una
// clara. Se miran antes que las reglas de texto: el código no cambia de
// redacción.
var hintsPorCodigo = map[api.ErrorCode]string{
	api.CodeDiskFull:           "kling ps -a and kling template ls show what holds disk; kling rm and kling template rm free it",
	api.CodeInsufficientMemory: "kling ps shows what is running; kling freeze <ref> gives an idle one's memory back (or ask for less with -mem)",
}

// reglasNombre son las de validName en internal/machine: lo que vale como
// nombre de imagen, plantilla o snapshot.
const reglasNombre = "names: 1-64 characters, letters, digits, '.', '_' and '-', starting with a letter or digit"

// hint es una regla: si el error contiene todas las piezas de match, la pista
// es try, salvo que el mensaje ya contenga salvo (ya lo explica).
type hint struct {
	match []string
	try   string
	salvo string
}

// hints se recorre en orden y gana la primera que encaja: las más específicas
// van antes (un socket sin permiso también es un "cannot talk to the daemon").
var hints = []hint{
	{[]string{"cannot talk to the daemon", "permission denied"},
		"add your user to KLING_SOCKET_USER in /etc/default/kling and restart the daemon (or use sudo)", ""},
	{[]string{"cannot talk to the daemon"}, "kling doctor", ""},
	{[]string{"launching ssh to"}, "kling doctor", ""},
	{[]string{"machine ", " does not exist"}, "kling ps -a", ""},
	{[]string{"snapshot ", " does not exist"}, "kling template ls", ""},
	{[]string{`image "toolchain" does not exist`}, "kling image toolchain   (builds it)", ""},
	{[]string{"image ", " does not exist"}, "kling image ls", ""},
	{[]string{"volume ", " not found"}, "kling volume ls", ""},
	{[]string{"context ", " does not exist"}, "kling context ls", ""},
	{[]string{"missing kernel at"}, "kling up -check   (it says how to get the kernel)", ""},
	{[]string{"invalid volume name"}, "volume names: lowercase letters, digits, '-' and '_'", ": lowercase"},
	{[]string{"invalid image name"}, reglasNombre, ""},
	{[]string{"invalid snapshot name"}, reglasNombre, ": lowercase"},
	{[]string{"invalid name \""}, reglasNombre, ""},
	{[]string{"404 page not found"}, "kling version   (the daemon may be older than this kling)", ""},
	// Un daemon que se cae a media petición, o un ssh que conecta y no
	// encuentra kling al otro lado, llegan como un EOF del cliente HTTP.
	{[]string{`"http://kling`, "EOF"}, "kling doctor", ""},
	{[]string{`"http://kling`, "connection reset"}, "kling doctor", ""},
}

// hintFor devuelve el siguiente paso para un error, o "" si no hay uno claro.
// Un error que ya dice qué hacer (lleva "kling " dentro) no recibe otra: dos
// consejos distintos confunden más que uno.
func hintFor(err error) string {
	if err == nil {
		return ""
	}
	var wh *errWithHint
	if errors.As(err, &wh) {
		return wh.hint
	}
	var uso *errorUso
	if errors.As(err, &uso) {
		return uso.hint()
	}
	// 429 guest_busy: el agente está vivo pero lleno (64 exec o 8 shells a la
	// vez). Sale con 1, como cualquier error; la pista dice que no es una
	// caída y que basta con reintentar.
	if api.IsGuestBusy(err) {
		return "retry when some of the commands or shells already running in that microVM finish"
	}
	msg := err.Error()
	yaDice := strings.Contains(msg, "`kling ") || strings.Contains(msg, "with:  kling")
	if h, ok := hintsPorCodigo[api.CodeOf(err)]; ok {
		if yaDice {
			return ""
		}
		return h
	}
	for _, h := range hints {
		if containsAll(msg, h.match) {
			if yaDice || h.salvo != "" && strings.Contains(msg, h.salvo) {
				return ""
			}
			return h.try
		}
	}
	// Un 404 del API sin texto propio es casi siempre un daemon anterior al
	// CLI que no conoce la ruta.
	var se *api.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound && strings.HasPrefix(se.Message, "404") {
		return "kling version   (the daemon may be older than this kling)"
	}
	return ""
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// printError es como main informa de un fallo: "error: …" y, si la hay, la
// pista en una segunda línea que empieza por "try:", fácil de ver y de buscar.
func printError(w io.Writer, err error) {
	if err.Error() == "" {
		return // el comando ya lo contó (doctor): solo cuenta el código de salida
	}
	fmt.Fprintln(w, "error:", err)
	if h := hintFor(err); h != "" {
		fmt.Fprintln(w, "try:", h)
	}
}

// movedToExtension son palabras de primer nivel que aporta una extensión.
// Quien teclea `kling mcp add` (o el alias `kling add`) sin tenerla instalada
// no necesita la ayuda entera: se le dice qué instalar.
var movedToExtension = map[string]string{
	"mcp":     "mcp",
	"connect": "mcp",
	"db":      "db",
}
