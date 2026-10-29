package api

// Versión del API del daemon (docs/actualizar.md, §3.3).
//
// Lo nuevo se anuncia en Info.Capabilities y no cambia nada de esto.
// APIVersion sube cuando un cliente de antes ya no puede fiarse de lo que
// sabía, es decir, cuando se QUITA o se CAMBIA (nunca al añadir):
//
//   - una ruta, o la acción de autorización con la que se sirve
//     (internal/daemon/testdata/contrato/rutas.txt);
//   - el código de estado o el code de error que contesta una ruta
//     documentada (internal/daemon/testdata/contrato/codigos.txt);
//   - un campo JSON de un tipo de este paquete (nombre, opciones o tipo), un
//     tipo exportado o el valor de una constante de cadena: estados,
//     capacidades, códigos de error (pkg/api/testdata/json-tags.txt);
//   - un campo de la salida -json del CLI (cmd/kling/testdata/contrato/json.txt);
//   - la semántica de una ruta aunque no cambie su forma (p. ej. que commit
//     pase a esperar "ready"): esto no lo ve ningún golden, es de quien revisa.
//
// Los goldens apuntan el APIVersion con el que se generaron ("# api: N") y
// internal/contrato hace cumplir la regla: si una línea desaparece o cambia
// sin que APIVersion suba, el test falla y -update se niega; al subirla hay
// que regenerarlos todos. Subir APIVersion es un MAJOR del contrato 1.x
// (docs/compatibilidad.md). El daemon la manda en
// Info.API y, para que la vea cualquier petición sin preguntar /info, en la
// cabecera X-Kling-API de cada respuesta. El cliente la compara:
//
//   - daemon con un API mayor que la del cliente: aviso, una vez (el cliente es
//     más viejo que el daemon; lo que se quitó fallará);
//   - daemon con un API menor que MinDaemonAPI: error, sin intentar la petición
//     que seguramente no entendería.
//
// Un daemon anterior no manda ni el campo ni la cabecera: es el API 1, el de
// siempre.

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// APIVersion es el API del daemon que habla este paquete.
const APIVersion = 1

// MinDaemonAPI es el API más viejo de daemon con el que este cliente funciona.
// Es variable solo para poder probar el rechazo; no se toca en producción.
var MinDaemonAPI = 1

// Cabeceras de cada respuesta del daemon.
const (
	HeaderAPI     = "X-Kling-API"
	HeaderVersion = "X-Kling-Version"
)

// APIWarning recibe el aviso de un daemon con un API más nuevo. Por defecto va
// a stderr: el CLI y las extensiones son programas de terminal, y un gateway lo
// deja en su journal.
var APIWarning = func(msg string) { fmt.Fprintln(os.Stderr, "warning: "+msg) }

// DaemonAPI es el API que anuncia info; 0 (un daemon anterior) es 1.
func (i *Info) DaemonAPI() int {
	if i == nil || i.API <= 0 {
		return 1
	}
	return i.API
}

// ErrDaemonTooOld es el error de un daemon con un API más viejo del que este
// cliente necesita.
type ErrDaemonTooOld struct {
	Version string // la del daemon; vacía si no la dijo
	API     int
}

func (e *ErrDaemonTooOld) Error() string {
	v := e.Version
	if v == "" {
		v = "older than v0.18"
	}
	return fmt.Sprintf("the daemon is %s (API %d) and this kling needs API %d or newer: "+
		"update the daemon on its host (scripts/install.sh, then restart it: "+
		"`sudo systemctl restart kling`)", v, e.API, MinDaemonAPI)
}

// CheckAPI compara el API de un daemon con la de este cliente: aviso si el
// daemon es más nuevo, error si es más viejo que MinDaemonAPI.
func CheckAPI(daemonAPI int, daemonVersion string) (warning string, err error) {
	if daemonAPI <= 0 {
		daemonAPI = 1
	}
	if daemonAPI < MinDaemonAPI {
		return "", &ErrDaemonTooOld{Version: daemonVersion, API: daemonAPI}
	}
	if daemonAPI > APIVersion {
		v := daemonVersion
		if v == "" {
			v = "newer"
		}
		return fmt.Sprintf("the daemon (%s) speaks API %d and this client knows up to %d: "+
			"what changed may fail; update this kling and its extensions to the daemon's version",
			v, daemonAPI, APIVersion), nil
	}
	return "", nil
}

// comprobarAPI envuelve el transporte del cliente y mira la cabecera X-Kling-API
// de cada respuesta: así lo comprueba toda petición, incluidas las que no pasan
// por doWith (flujos, blobs), sin una ida y vuelta de más a /info.
type comprobarAPI struct {
	rt     http.RoundTripper
	avisar *sync.Once
}

func (t comprobarAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.rt.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	n := 1 // sin cabecera: daemon anterior, API 1
	if h := resp.Header.Get(HeaderAPI); h != "" {
		if v, perr := strconv.Atoi(h); perr == nil {
			n = v
		}
	}
	warn, cerr := CheckAPI(n, resp.Header.Get(HeaderVersion))
	if cerr != nil {
		resp.Body.Close()
		return nil, cerr
	}
	if warn != "" {
		t.avisar.Do(func() { APIWarning(warn) })
	}
	return resp, nil
}
