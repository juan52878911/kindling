package api

// Versión del API del daemon (docs/actualizar.md, §3.3).
//
// Las rutas nuevas se anuncian en Info.Capabilities y no cambian nada de esto.
// APIVersion sube solo cuando se QUITA o se CAMBIA una ruta: entonces un
// cliente de antes ya no puede fiarse de lo que sabía. El daemon la manda en
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
