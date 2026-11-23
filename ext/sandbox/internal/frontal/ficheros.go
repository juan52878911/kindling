package frontal

// Operaciones de ficheros que el invitado no tiene como llamada propia:
// mover, crear directorios y buscar, más el Range de la descarga. Se apoyan
// en lo que ya tiene api.Client (tar, stat, exec) y no tocan el agente.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// maxResultadosBusqueda acota las rutas que devuelve una búsqueda.
	maxResultadosBusqueda = 1000
	// plazoBusqueda es lo que puede tardar el find (segundos).
	plazoBusqueda = 20
	// salidaBusquedaMax acota los bytes de la salida del find.
	salidaBusquedaMax = 1 << 20
)

// BusquedaFicheros es la respuesta de GET .../files/search.
type BusquedaFicheros struct {
	Path      string   `json:"path"`
	Glob      string   `json:"glob"`
	Matches   []string `json:"matches"`
	Truncated bool     `json:"truncated,omitempty"`
}

// MoverPeticion es el cuerpo de POST .../files/move.
type MoverPeticion struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Overwrite pide pisar el destino si existe; sin él, un destino que existe
	// es un 409.
	Overwrite bool `json:"overwrite,omitempty"`
}

// MkdirPeticion es el cuerpo de POST .../files/mkdir.
type MkdirPeticion struct {
	Path string `json:"path"`
}

// despachaFicheros atiende /v1/sandboxes/{id}/files[/move|/mkdir|/search].
func (s *Servidor) despachaFicheros(w http.ResponseWriter, r *http.Request, id string, resto []string) {
	nomala := func() {
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	}
	switch {
	case len(resto) == 0:
		switch r.Method {
		case http.MethodGet, http.MethodPut, http.MethodDelete:
			s.handleFiles(w, r, id)
		default:
			nomala()
		}
	case len(resto) > 1:
		noRuta(w, r)
	case resto[0] == "move" && r.Method == http.MethodPost:
		s.handleMover(w, r, id)
	case resto[0] == "mkdir" && r.Method == http.MethodPost:
		s.handleMkdir(w, r, id)
	case resto[0] == "search" && r.Method == http.MethodGet:
		s.handleBuscar(w, r, id)
	case resto[0] == "move" || resto[0] == "mkdir" || resto[0] == "search":
		nomala()
	default:
		noRuta(w, r)
	}
}

// rutaAbsoluta valida una ruta de dentro del invitado: absoluta, sin NUL.
func rutaAbsoluta(campo, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%s is required", campo)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("%s must be an absolute path", campo)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%s has a NUL byte", campo)
	}
	return nil
}

func (s *Servidor) handleMover(w http.ResponseWriter, r *http.Request, id string) {
	var req MoverPeticion
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := errors.Join(rutaAbsoluta("from", req.From), rutaAbsoluta("to", req.To)); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	cmd := []string{"mv", "--", req.From, req.To}
	if !req.Overwrite {
		// Un destino que existe no se pisa sin pedirlo; "-n" cubre la carrera
		// entre la comprobación y el mv.
		if _, err := h.Cliente.StatFile(r.Context(), mc.ID, req.To); err == nil {
			fail(w, http.StatusConflict, fmt.Errorf("%s already exists: pass overwrite to replace it", req.To))
			return
		} else if !esNoEncontrado(err) {
			fail(w, codigoExec(err), deHost(h, err))
			return
		}
		cmd = []string{"mv", "-n", "--", req.From, req.To}
	}
	// Las rutas son absolutas, así que ninguna parece una opción; "--" lo
	// asegura igualmente.
	res, err := h.Cliente.Exec(r.Context(), mc.ID, api.ExecRequest{
		Cmd: cmd, TimeoutSeconds: 30, MaxOutputBytes: 64 << 10,
	}, nil)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		code := http.StatusConflict
		if strings.Contains(msg, "No such file") {
			code = http.StatusNotFound
		}
		fail(w, code, fmt.Errorf("move failed: %s", msg))
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// esNoEncontrado dice si el daemon contestó 404.
func esNoEncontrado(err error) bool {
	var se *api.StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// tarVacio es un tar sin entradas: extraerlo con mkdir=1 crea el directorio.
func tarVacio() []byte {
	var b bytes.Buffer
	_ = tar.NewWriter(&b).Close()
	return b.Bytes()
}

func (s *Servidor) handleMkdir(w http.ResponseWriter, r *http.Request, id string) {
	var req MkdirPeticion
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := rutaAbsoluta("path", req.Path); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	// Un tar vacío extraído con mkdir=1 crea el directorio y los que falten
	// (mkdir -p), sin ejecutar nada ni pasar por un shell.
	if _, err := h.Cliente.WriteTar(r.Context(), mc.ID, req.Path, true, bytes.NewReader(tarVacio())); err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	st, err := h.Cliente.StatFile(r.Context(), mc.ID, req.Path)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (s *Servidor) handleBuscar(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	raiz, glob := q.Get("path"), q.Get("glob")
	if err := rutaAbsoluta("path", raiz); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case glob == "":
		fail(w, http.StatusBadRequest, errors.New("glob is required"))
		return
	case strings.ContainsAny(glob, "/\x00"):
		fail(w, http.StatusBadRequest, errors.New("glob matches file names: it can't contain '/'"))
		return
	case len(glob) > 255:
		fail(w, http.StatusBadRequest, errors.New("glob is too long"))
		return
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	// find sale con 1 si la raíz no existe, igual que con un acceso denegado
	// a medias: se comprueba antes para dar el 404.
	if _, err := h.Cliente.StatFile(r.Context(), mc.ID, raiz); err != nil {
		if esNoEncontrado(err) {
			fail(w, http.StatusNotFound, fmt.Errorf("search failed: %s does not exist", raiz))
		} else {
			fail(w, codigoExec(err), deHost(h, err))
		}
		return
	}
	out, err := buscarFicheros(r.Context(), h, mc.ID, raiz, glob)
	if err != nil {
		var se *errBusqueda
		if errors.As(err, &se) {
			fail(w, se.estado, err)
			return
		}
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type errBusqueda struct {
	estado int
	msg    string
}

func (e *errBusqueda) Error() string { return e.msg }

// buscarFicheros corre find con plazo y tope de salida; las rutas llegan
// separadas por NUL (los nombres pueden llevar saltos de línea).
func buscarFicheros(ctx context.Context, h *hosts.Host, ref, raiz, glob string) (*BusquedaFicheros, error) {
	res, err := h.Cliente.Exec(ctx, ref, api.ExecRequest{
		Cmd:            []string{"find", raiz, "-name", glob, "-print0"},
		TimeoutSeconds: plazoBusqueda, MaxOutputBytes: salidaBusquedaMax,
	}, nil)
	if err != nil {
		return nil, err
	}
	out := &BusquedaFicheros{Path: raiz, Glob: glob, Matches: []string{}, Truncated: res.Truncated || res.TimedOut}
	switch {
	case res.ExitCode == 127:
		return nil, &errBusqueda{http.StatusUnprocessableEntity, "this sandbox image has no find"}
	case res.ExitCode > 1 && !res.TimedOut && !res.Truncated:
		msg := strings.TrimSpace(string(res.Stderr))
		code := http.StatusBadRequest
		if strings.Contains(msg, "No such file") {
			code = http.StatusNotFound
		}
		return nil, &errBusqueda{code, "search failed: " + msg}
	}
	for _, p := range bytes.Split(res.Stdout, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		if len(out.Matches) == maxResultadosBusqueda {
			out.Truncated = true
			break
		}
		out.Matches = append(out.Matches, string(p))
	}
	// Con la salida cortada la última ruta puede estar a medias.
	if res.Truncated && len(out.Matches) > 0 && !bytes.HasSuffix(res.Stdout, []byte{0}) {
		out.Matches = out.Matches[:len(out.Matches)-1]
	}
	return out, nil
}

// descargaConRango sirve un Range "bytes=" de un fichero. Devuelve false si
// el Range no se entiende (varios rangos, sintaxis rara): se sirve el fichero
// entero, que es lo que permite el RFC 9110. Si contesta (206 o 416), true.
func (s *Servidor) descargaConRango(w http.ResponseWriter, r *http.Request, h *hosts.Host, ref, ruta, rango string) bool {
	spec := strings.TrimPrefix(rango, "bytes=")
	if strings.Contains(spec, ",") {
		return false
	}
	ini, fin, ok := strings.Cut(spec, "-")
	if !ok {
		return false
	}
	st, err := h.Cliente.StatFile(r.Context(), ref, ruta)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return true
	}
	size := st.Size
	// El daemon no sirve más de FileMaxDownload: ni se anuncia más ni se
	// intenta empezar más allá.
	tope := min(size, api.FileMaxDownload)
	var a, b int64
	switch {
	case ini == "" && fin != "": // últimos n bytes
		n, err := strconv.ParseInt(fin, 10, 64)
		if err != nil || n < 0 {
			return false
		}
		if n == 0 {
			return rangoNoSatisfecho(w, size)
		}
		a, b = max(size-n, 0), size-1 // el tope se aplica abajo
	case ini != "":
		var err1, err2 error
		a, err1 = strconv.ParseInt(ini, 10, 64)
		b = size - 1
		if fin != "" {
			b, err2 = strconv.ParseInt(fin, 10, 64)
		}
		if err1 != nil || err2 != nil || a < 0 || (fin != "" && b < a) {
			return false
		}
		b = min(b, size-1)
	default:
		return false
	}
	if a >= tope {
		return rangoNoSatisfecho(w, size)
	}
	b = min(b, tope-1)
	rc, err := h.Cliente.ReadFile(r.Context(), ref, ruta)
	if err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return true
	}
	defer rc.Close()
	// El invitado no sabe saltar: se descarta lo anterior. Cuesta lo que el
	// desplazamiento, pero no pasa de FileMaxDownload.
	if _, err := io.CopyN(io.Discard, rc, a); err != nil {
		fail(w, http.StatusBadGateway, deHost(h, err))
		return true
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, size))
	w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.CopyN(w, rc, b-a+1)
	return true
}

func rangoNoSatisfecho(w http.ResponseWriter, size int64) bool {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	fail(w, http.StatusRequestedRangeNotSatisfiable, errors.New("the range is outside the file"))
	return true
}
