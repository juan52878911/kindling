package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
)

// CREDENCIALES DE REGISTROS PRIVADOS.
//
// El constructor oci baja de un registro privado con las credenciales que el
// administrador deja en el daemon (`kling registry login`), no en el spec de
// la construcción: el spec va entero a la receta, y la receta se copia entre
// daemons y se lee con `kling image recipe`.
//
// Viven en <root>/registries.json, de root y 0600, y no salen de ahí más que
// de dos formas: GET /registries da los hosts y los usuarios, nunca la
// contraseña; y cada construcción oci recibe SOLO las del registro de su
// referencia, en un fichero 0600 suyo en su directorio de trabajo
// (escribirCredencialesConstructor), que lee y borra al empezar. Ni por argv
// ni por el entorno (el de un constructor sin root es de lista blanca, y el
// de un proceso se lee en /proc/<pid>/environ), ni en request.json, la
// receta, el log de la construcción, state.json o los eventos.

// ficheroRegistros es el nombre del fichero en la raíz de datos.
const ficheroRegistros = "registries.json"

// FicheroCredencialesConstructor es el fichero, en el directorio de trabajo,
// con las credenciales del registro de la imagen que se construye.
const FicheroCredencialesConstructor = "registry-auth.json"

// maxPassword acota una contraseña o un token (los de ECR rondan los 2 KiB, un
// JWT de un servicio de tokens algo más).
const maxPassword = 16 << 10

// registrosGuardados es el formato de registries.json.
type registrosGuardados struct {
	Auths map[string]oci.Credential `json:"auths"`
}

func (s *Server) rutaRegistros() string { return filepath.Join(s.root, ficheroRegistros) }

// leerRegistros lee las credenciales guardadas. Sin fichero, ninguna.
func (s *Server) leerRegistros() (map[string]oci.Credential, error) {
	b, err := os.ReadFile(s.rutaRegistros())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]oci.Credential{}, nil
	}
	if err != nil {
		return nil, err
	}
	var g registrosGuardados
	if err := json.Unmarshal(b, &g); err != nil {
		// Sin el error de json: podría citar un trozo del fichero.
		return nil, fmt.Errorf("%s is not valid JSON", s.rutaRegistros())
	}
	if g.Auths == nil {
		g.Auths = map[string]oci.Credential{}
	}
	return g.Auths, nil
}

func (s *Server) guardarRegistros(m map[string]oci.Credential) error {
	b, err := json.MarshalIndent(registrosGuardados{Auths: m}, "", "  ")
	if err != nil {
		return err
	}
	if err := durable.Escribir(s.rutaRegistros(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	// Escribir respeta el umask y los permisos de un fichero que ya estaba: se
	// dejan en 0600 siempre.
	return os.Chmod(s.rutaRegistros(), 0o600)
}

// validarCredencial comprueba lo que llega por el API. Los errores no citan
// la contraseña.
func validarCredencial(req api.RegistryLoginRequest) (string, error) {
	host, err := oci.CredentialKey(req.Host)
	if err != nil {
		return "", err
	}
	if len(req.Username) > 256 || strings.ContainsFunc(req.Username, esControl) || strings.Contains(req.Username, ":") {
		return "", errors.New("invalid username (at most 256 characters, no ':' or control characters)")
	}
	if req.Password == "" {
		return "", errors.New("the password or token is empty")
	}
	if len(req.Password) > maxPassword || strings.ContainsFunc(req.Password, esControl) {
		return "", fmt.Errorf("invalid password or token (at most %d bytes, one line)", maxPassword)
	}
	return host, nil
}

func esControl(r rune) bool { return r < 0x20 || r == 0x7f }

func (s *Server) handleRegistries(w http.ResponseWriter, r *http.Request) {
	s.muRegistros.Lock()
	m, err := s.leerRegistros()
	s.muRegistros.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []api.Registry{}
	for h, c := range m {
		out = append(out, api.Registry{Host: h, Username: c.Username})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRegistryLogin(w http.ResponseWriter, r *http.Request) {
	var req api.RegistryLoginRequest
	if err := decodeJSONCon(w, r, &req, 64<<10); err != nil {
		fail(w, jsonBodyStatus(err), errors.New("invalid registry login request"))
		return
	}
	host, err := validarCredencial(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.muRegistros.Lock()
	defer s.muRegistros.Unlock()
	m, err := s.leerRegistros()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	m[host] = oci.Credential{Username: req.Username, Password: req.Password}
	if err := s.guardarRegistros(m); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("registry credentials for %s saved (user %q)", host, req.Username)
	writeJSON(w, http.StatusOK, api.Registry{Host: host, Username: req.Username})
}

func (s *Server) handleRegistryLogout(w http.ResponseWriter, r *http.Request) {
	host, err := oci.CredentialKey(r.PathValue("host"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.muRegistros.Lock()
	defer s.muRegistros.Unlock()
	m, err := s.leerRegistros()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if _, ok := m[host]; !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no credentials saved for %s", host))
		return
	}
	delete(m, host)
	if err := s.guardarRegistros(m); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("registry credentials for %s removed", host)
	w.WriteHeader(http.StatusNoContent)
}

// credencialesConstructor da, para una construcción oci, las credenciales
// del registro de su referencia ya en el formato del fichero que lee el
// constructor; nil si no hay (o si el spec no se entiende: el constructor
// dará el error).
func (s *Server) credencialesConstructor(req api.BuildImageRequest) ([]byte, error) {
	if req.Builder != "oci" || len(req.Spec) == 0 {
		return nil, nil
	}
	var spec struct {
		Ref string `json:"ref"`
	}
	if json.Unmarshal(req.Spec, &spec) != nil {
		return nil, nil
	}
	ref, err := oci.ParseImageRef(spec.Ref)
	if err != nil {
		return nil, nil
	}
	host, err := oci.CredentialKey(ref.Registry)
	if err != nil {
		return nil, nil
	}
	s.muRegistros.Lock()
	m, err := s.leerRegistros()
	s.muRegistros.Unlock()
	if err != nil {
		return nil, err
	}
	c, ok := m[host]
	if !ok {
		return nil, nil
	}
	return json.Marshal(map[string]oci.Credential{host: c})
}

// escribirCredencialesConstructor deja auth en el directorio de trabajo, 0600
// y del usuario de construcción (si lo hay). Va antes de ceder el directorio
// (prepararTrabajo): mientras es de root nadie más puede plantar ahí un
// enlace.
func escribirCredencialesConstructor(work string, auth []byte, u *usuarioConstructor) error {
	p := filepath.Join(work, FicheroCredencialesConstructor)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(auth)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && u != nil {
		err = os.Lchown(p, int(u.UID), int(u.GID))
	}
	if err != nil {
		os.Remove(p)
	}
	return err
}
