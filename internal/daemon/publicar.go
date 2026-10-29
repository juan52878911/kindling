package daemon

// Los puertos publicados por el daemon (internal/publicar): POST/DELETE
// /machines/{ref}/publish, GET /publish y RunRequest.Publish. Ver
// docs/desarrollo-local.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/internal/publicar"
	"github.com/juan52878911/kindling/pkg/api"
)

func init() { Capabilities = append(Capabilities, api.CapabilityPublish) }

// gestorPublicar adapta el manager a publicar.Maquinas.
type gestorPublicar struct{ *machine.Manager }

// publicador devuelve el de s, creándolo la primera vez (los tests que no
// pasan por Listen también lo tienen).
func (s *Server) publicador() *publicar.Publicador {
	s.pubUna.Do(func() {
		s.pub = publicar.New(gestorPublicar{s.mgr}, filepath.Join(s.root, "publish.json"), log.Printf)
		s.pub.SetActividad(func(id string) publicar.Actividad {
			n, u, ses := s.mgr.Actividad(id)
			return publicar.Actividad{EnCurso: n, Ultima: u, Sesiones: ses}
		})
	})
	return s.pub
}

// SetIdleFreeze fija de dónde se lee daemon.idle_freeze (0 = nunca). Se
// consulta en cada vuelta: cambiarlo no pide reiniciar.
func (s *Server) SetIdleFreeze(f func() time.Duration) { s.idleFreeze = f }

// arrancarPublicador recupera los puertos publicados antes del reinicio y
// lanza su barrido (y el de idle_freeze).
func (s *Server) arrancarPublicador(ctx context.Context) {
	s.publicador().Arrancar(ctx, s.idleFreeze)
}

// comprobarPuertoBajo: con política de autorización, un inquilino no publica
// puertos privilegiados del host (el daemon corre como root en Linux).
func comprobarPuertoBajo(r *http.Request, ps []api.PortPublish) error {
	if _, filtra := inquilinoDe(r); !filtra {
		return nil
	}
	for _, p := range ps {
		if p.Host != 0 && p.Host < 1024 {
			return fmt.Errorf("host port %d: below 1024 only an admin can publish", p.Host)
		}
	}
	return nil
}

func publicarStatus(err error) int {
	switch {
	case errors.Is(err, publicar.ErrOcupado):
		return http.StatusConflict
	case errors.Is(err, publicar.ErrNoSoportado):
		return http.StatusNotImplemented
	}
	return http.StatusBadRequest
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	mc, ok := s.mgr.Get(r.PathValue("ref"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("that machine doesn't exist"))
		return
	}
	var req api.PublishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	if len(req.Ports) == 0 {
		fail(w, http.StatusBadRequest, errors.New("no ports to publish"))
		return
	}
	if err := comprobarPuertoBajo(r, req.Ports); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	pub, err := s.publicador().Publicar(mc, req.Ports)
	if err != nil {
		fail(w, publicarStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	id := ref
	if mc, ok := s.mgr.Get(ref); ok {
		id = mc.ID
	} else if s.publicador().De(ref) == nil {
		fail(w, http.StatusNotFound, errors.New("that machine doesn't exist"))
		return
	}
	var hosts []int
	if v := r.URL.Query().Get("host"); v != "" {
		for _, x := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(x))
			if err != nil {
				fail(w, http.StatusBadRequest, fmt.Errorf("host=%s: not a port", x))
				return
			}
			hosts = append(hosts, n)
		}
	}
	s.publicador().Quitar(id, hosts...)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePublications(w http.ResponseWriter, r *http.Request) {
	l := s.publicador().Lista()
	if t, filtra := inquilinoDe(r); filtra {
		var mias []api.Publication
		for _, p := range l {
			if mc, ok := s.mgr.Get(p.Machine); ok && esDe(mc.Labels, t) {
				mias = append(mias, p)
			}
		}
		l = mias
	}
	if l == nil {
		l = []api.Publication{}
	}
	writeJSON(w, http.StatusOK, l)
}

// conPublicacion envuelve POST /machines: con RunRequest.Publish, comprueba
// los puertos ANTES de crear la máquina (uno ocupado falla sin arrancar nada)
// y los publica en cuanto existe. Si al final no se pueden publicar (otro los
// tomó entre medias), la máquina se borra: todos o nada.
func (s *Server) conPublicacion(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cuerpo, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jsonMaxBody))
		if err != nil {
			fail(w, jsonBodyStatus(err), err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(cuerpo))
		var pide struct {
			Publish []api.PortPublish `json:"publish"`
		}
		if json.Unmarshal(cuerpo, &pide) != nil || len(pide.Publish) == 0 {
			h(w, r)
			return
		}
		if err := comprobarPuertoBajo(r, pide.Publish); err != nil {
			fail(w, http.StatusForbidden, err)
			return
		}
		pub := s.publicador()
		if err := pub.Comprobar(pide.Publish); err != nil {
			fail(w, publicarStatus(err), err)
			return
		}
		cap := &capturaRespuesta{cab: http.Header{}}
		h(cap, r)
		if cap.codigo != http.StatusCreated {
			cap.volcar(w)
			return
		}
		var mc api.Machine
		if err := json.Unmarshal(cap.cuerpo.Bytes(), &mc); err != nil {
			cap.volcar(w)
			return
		}
		cur, ok := s.mgr.Get(mc.ID)
		if !ok {
			cap.volcar(w)
			return
		}
		p, err := pub.Publicar(cur, pide.Publish)
		if err != nil {
			if rerr := s.mgr.Remove(mc.ID); rerr != nil {
				log.Printf("publish: removing %s after failing to publish its ports: %v", mc.Name, rerr)
			}
			fail(w, publicarStatus(err), fmt.Errorf("%s was not kept: %w", mc.Name, err))
			return
		}
		mc.Published = p.Ports
		writeJSON(w, http.StatusCreated, &mc)
	}
}

// capturaRespuesta guarda la respuesta de un manejador para mirarla antes de
// mandarla.
type capturaRespuesta struct {
	cab    http.Header
	codigo int
	cuerpo bytes.Buffer
}

func (c *capturaRespuesta) Header() http.Header { return c.cab }
func (c *capturaRespuesta) Write(b []byte) (int, error) {
	if c.codigo == 0 {
		c.codigo = http.StatusOK
	}
	return c.cuerpo.Write(b)
}
func (c *capturaRespuesta) WriteHeader(code int) {
	if c.codigo == 0 {
		c.codigo = code
	}
}

func (c *capturaRespuesta) volcar(w http.ResponseWriter) {
	for k, v := range c.cab {
		w.Header()[k] = v
	}
	if c.codigo == 0 {
		c.codigo = http.StatusOK
	}
	w.WriteHeader(c.codigo)
	_, _ = w.Write(c.cuerpo.Bytes())
}
