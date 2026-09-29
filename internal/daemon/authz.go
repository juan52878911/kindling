package daemon

// Autorización por operación en el socket del daemon (docs/authz.md, #78).
//
// Sin política, quien alcanza el socket manda sobre todo, como siempre: es lo
// correcto en un host de un solo usuario y lo que esperan las instalaciones
// que ya existen. Con política (/etc/kling/authz.json, o -authz), el daemon
// mira quién está al otro lado del socket (SO_PEERCRED en Linux,
// LOCAL_PEERCRED en macOS; por SSH es el usuario remoto, que es quien lanza
// `kling dial-stdio`) y le da un rol:
//
//   - admin: todo, como sin política.
//   - tenant:<nombre>: solo ve y opera las máquinas, snapshots y grafos que
//     llevan kling.owner=<nombre>. Esa etiqueta la pone el daemon al crear, y
//     un inquilino no puede fijarla ni cambiarla.
//   - ninguno: 403 en todo.
//
// Se aplica en UN sitio: cada ruta declara su acción (ver rutas() en
// server.go) y autorizar decide antes de llamar al handler. Los handlers solo
// filtran sus listados con lo que autorizar dejó en el contexto.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// RutaAuthzPorDefecto es dónde se busca la política si no se indica otra. Que
// no exista ahí no es un error: es el modo de siempre, sin autorización.
const RutaAuthzPorDefecto = "/etc/kling/authz.json"

// authzMaxFichero acota el fichero de política: unas decenas de reglas caben
// de sobra.
const authzMaxFichero = 1 << 20

// Llamante es quien está al otro lado del socket.
type Llamante struct {
	UID, GID int
	// Groups son sus grupos si el sistema los da con la conexión (macOS). nil
	// = no se saben: se resuelven por la base de usuarios si hace falta.
	Groups []int
	// Conocido: se pudieron leer sus credenciales. Sin ellas no hay rol.
	Conocido bool
}

// Rol es lo que la política concede a quien llama.
type Rol struct {
	Admin  bool
	Tenant string
}

// Valido dice si el rol concede algo.
func (r Rol) Valido() bool { return r.Admin || r.Tenant != "" }

func (r Rol) String() string {
	switch {
	case r.Admin:
		return "admin"
	case r.Tenant != "":
		return "tenant:" + r.Tenant
	}
	return "none"
}

// parseRol entiende "admin" y "tenant:<nombre>".
func parseRol(s string) (Rol, error) {
	if s == "admin" {
		return Rol{Admin: true}, nil
	}
	if t, ok := strings.CutPrefix(s, "tenant:"); ok {
		if !api.KeyPattern.MatchString(t) {
			return Rol{}, fmt.Errorf("tenant name %q is not valid (lowercase letters, digits, '.', '_', '-')", t)
		}
		return Rol{Tenant: t}, nil
	}
	return Rol{}, fmt.Errorf("unknown role %q: use \"admin\" or \"tenant:<name>\"", s)
}

// ── la política ──────────────────────────────────────────────────────────────

// ficheroAuthz es el formato del fichero (docs/authz.md).
type ficheroAuthz struct {
	Rules []struct {
		UID   *int   `json:"uid,omitempty"`
		User  string `json:"user,omitempty"`
		GID   *int   `json:"gid,omitempty"`
		Group string `json:"group,omitempty"`
		Role  string `json:"role"`
	} `json:"rules"`
	// SharedTemplates son snapshots SIN dueño (los de un admin) que todos los
	// inquilinos pueden ver y usar como plantilla, sin tocarlos.
	SharedTemplates []string `json:"shared_templates,omitempty"`
	// Tokens: el sha256 (hex) de un token y el inquilino que da. Solo roles de
	// inquilino: admin se es por usuario del sistema, nunca por un secreto.
	Tokens []struct {
		SHA256 string `json:"sha256"`
		Role   string `json:"role"`
	} `json:"tokens,omitempty"`
}

type reglaAuthz struct {
	porUID bool
	id     int // uid o gid
	rol    Rol
}

type tokenAuthz struct {
	hash [sha256.Size]byte
	rol  Rol
}

// Politica es la política ya validada. Es inmutable: cambiarla pide reiniciar
// el daemon.
type Politica struct {
	Ruta        string
	reglas      []reglaAuthz
	compartidas map[string]bool
	tokens      []tokenAuthz
	// admins son los uid que siempre son admin: root y el usuario del daemon.
	// Cualquiera de los dos puede reescribir la política o el daemon mismo, así
	// que negarles algo no protegería nada.
	admins map[int]bool
	// grupos resuelve los grupos suplementarios de un uid cuando la conexión
	// no los trae (Linux). Variable para los tests.
	grupos func(uid int) []int
}

// resolutor traduce nombres de usuario y de grupo a ids. Los tests pasan uno
// propio.
type resolutor struct {
	usuario func(string) (int, error)
	grupo   func(string) (int, error)
}

var resolutorSistema = resolutor{
	usuario: func(n string) (int, error) {
		u, err := user.Lookup(n)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(u.Uid)
	},
	grupo: func(n string) (int, error) {
		g, err := user.LookupGroup(n)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(g.Gid)
	},
}

// gruposSistema son los grupos suplementarios de uid según la base de
// usuarios. Un error deja la lista vacía: una regla de grupo no casa, que es
// lo seguro.
func gruposSistema(uid int) []int {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return nil
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil
	}
	var out []int
	for _, s := range ids {
		if n, err := strconv.Atoi(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// parsePolitica valida el contenido de un fichero de política. Todo lo que no
// se entiende es un error: una regla mal escrita que se ignorara en silencio
// dejaría a alguien sin el límite que se le quería poner.
func parsePolitica(b []byte, res resolutor) (*Politica, error) {
	var f ficheroAuthz
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the policy object")
	}
	p := &Politica{compartidas: map[string]bool{}, admins: map[int]bool{0: true, os.Geteuid(): true}, grupos: gruposSistema}
	for i, r := range f.Rules {
		n := 0
		for _, set := range []bool{r.UID != nil, r.User != "", r.GID != nil, r.Group != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			return nil, fmt.Errorf("rule %d: give exactly one of uid, user, gid or group", i+1)
		}
		rol, err := parseRol(r.Role)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		rg := reglaAuthz{rol: rol}
		switch {
		case r.UID != nil:
			rg.porUID, rg.id = true, *r.UID
		case r.User != "":
			id, err := res.usuario(r.User)
			if err != nil {
				return nil, fmt.Errorf("rule %d: user %q: %w", i+1, r.User, err)
			}
			rg.porUID, rg.id = true, id
		case r.GID != nil:
			rg.id = *r.GID
		default:
			id, err := res.grupo(r.Group)
			if err != nil {
				return nil, fmt.Errorf("rule %d: group %q: %w", i+1, r.Group, err)
			}
			rg.id = id
		}
		if rg.id < 0 {
			return nil, fmt.Errorf("rule %d: negative id", i+1)
		}
		p.reglas = append(p.reglas, rg)
	}
	for _, n := range f.SharedTemplates {
		if n == "" || strings.ContainsAny(n, "/\\") {
			return nil, fmt.Errorf("shared template %q is not a snapshot name", n)
		}
		p.compartidas[n] = true
	}
	for i, t := range f.Tokens {
		rol, err := parseRol(t.Role)
		if err != nil {
			return nil, fmt.Errorf("token %d: %w", i+1, err)
		}
		if rol.Admin {
			return nil, fmt.Errorf("token %d: a token can only give a tenant role; admin comes from the system user", i+1)
		}
		h, err := hex.DecodeString(t.SHA256)
		if err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("token %d: sha256 must be 64 hex characters (sha256sum of the token)", i+1)
		}
		var tk tokenAuthz
		copy(tk.hash[:], h)
		tk.rol = rol
		p.tokens = append(p.tokens, tk)
	}
	return p, nil
}

// CargarPolitica lee la política de ruta. Si no existe y no es obligatoria
// (la ruta por defecto), devuelve nil: sin autorización, como siempre. Si se
// pidió de forma explícita, que falte es un error: el daemon no debe arrancar
// abierto creyendo que está cerrado.
//
// El fichero tiene que ser regular (no un enlace), de root o del usuario del
// daemon, y no escribible por grupo ni otros: quien pudiera reescribirlo se
// daría admin.
func CargarPolitica(ruta string, obligatoria bool) (*Politica, error) {
	// Se abre el fichero sin seguir enlaces y se comprueba el descriptor
	// abierto (no un Lstat previo del nombre): así no hay hueco entre lo que
	// se comprueba y lo que se lee en el que cambiar el fichero por otro.
	f, err := abrirSinEnlace(ruta)
	if errors.Is(err, os.ErrNotExist) && !obligatoria {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authz policy: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("authz policy: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("authz policy %s: not a regular file", ruta)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("authz policy %s: writable by group or others (chmod 0644 or 0600)", ruta)
	}
	uid, ok := dueñoFichero(fi)
	if !ok {
		return nil, fmt.Errorf("authz policy %s: can't tell who owns it", ruta)
	}
	if uid != 0 && uid != os.Geteuid() {
		return nil, fmt.Errorf("authz policy %s: owned by uid %d; it must belong to root or to the daemon's user", ruta, uid)
	}
	b, err := io.ReadAll(io.LimitReader(f, authzMaxFichero+1))
	if err != nil {
		return nil, fmt.Errorf("authz policy: %w", err)
	}
	if len(b) > authzMaxFichero {
		return nil, fmt.Errorf("authz policy %s: larger than %d bytes", ruta, authzMaxFichero)
	}
	p, err := parsePolitica(b, resolutorSistema)
	if err != nil {
		return nil, fmt.Errorf("authz policy %s: %w", ruta, err)
	}
	p.Ruta = ruta
	return p, nil
}

// rol es el rol de un llamante: admin implícito, o la primera regla que case,
// en el orden del fichero.
func (p *Politica) rol(ll Llamante) Rol {
	if !ll.Conocido {
		return Rol{}
	}
	if p.admins[ll.UID] {
		return Rol{Admin: true}
	}
	var grupos []int
	resueltos := false
	for _, rg := range p.reglas {
		if rg.porUID {
			if rg.id == ll.UID {
				return rg.rol
			}
			continue
		}
		if !resueltos {
			grupos, resueltos = p.gruposDe(ll), true
		}
		if slices.Contains(grupos, rg.id) {
			return rg.rol
		}
	}
	return Rol{}
}

func (p *Politica) gruposDe(ll Llamante) []int {
	if ll.Groups != nil {
		return append([]int{ll.GID}, ll.Groups...)
	}
	out := []int{ll.GID}
	if p.grupos != nil {
		out = append(out, p.grupos(ll.UID)...)
	}
	return out
}

// rolDeToken busca el token entre los de la política. Compara con todos, sin
// cortar en el primero que case, como el frontal de ext/sandbox.
func (p *Politica) rolDeToken(tok string) (Rol, bool) {
	h := sha256.Sum256([]byte(tok))
	var rol Rol
	ok := false
	for _, t := range p.tokens {
		if subtle.ConstantTimeCompare(h[:], t.hash[:]) == 1 {
			rol, ok = t.rol, true
		}
	}
	return rol, ok
}

// snapVisible dice si el inquilino t puede ver y usar el snapshot name: si es
// suyo, o si no es de nadie y la política lo comparte.
func (p *Politica) snapVisible(name string, s *api.Snapshot, t string) bool {
	o := s.Labels[api.LabelOwner]
	return o == t || (o == "" && p.compartidas[name])
}

// ── identidad por conexión ───────────────────────────────────────────────────

type claveLlamante struct{}
type claveRol struct{}

// conContexto es el ConnContext del servidor: lee una vez, al aceptar la
// conexión, quién está al otro lado. s.identificar lo sustituye en los tests
// (un peercred falso).
func (s *Server) conContexto(ctx context.Context, c net.Conn) context.Context {
	id := s.identificar
	if id == nil {
		id = credencialesPar
	}
	ll, err := id(c)
	if err != nil {
		ll = Llamante{}
	}
	return context.WithValue(ctx, claveLlamante{}, ll)
}

func llamanteDe(ctx context.Context) Llamante {
	ll, _ := ctx.Value(claveLlamante{}).(Llamante)
	return ll
}

// rolDe es el rol que autorizar dejó en la petición. ok false = el daemon no
// tiene política.
func rolDe(r *http.Request) (Rol, bool) {
	rol, ok := r.Context().Value(claveRol{}).(Rol)
	return rol, ok
}

// inquilinoDe dice si hay que filtrar lo que ve r, y por qué dueño.
//
// Con filtrar true y t vacío (quien no tiene rol, que solo llega a GET /info)
// no se ve nada: lo que no tiene dueño tampoco es suyo.
func inquilinoDe(r *http.Request) (t string, filtrar bool) {
	rol, ok := rolDe(r)
	if !ok || rol.Admin {
		return "", false
	}
	return rol.Tenant, true
}

// esDe dice si algo con esas etiquetas es del inquilino t.
func esDe(labels map[string]string, t string) bool {
	return t != "" && labels[api.LabelOwner] == t
}

// ── acciones y rutas ─────────────────────────────────────────────────────────

// Accion es lo que una ruta hace, a efectos de autorización.
type Accion string

const (
	// AccionInfo: GET /info. Cualquiera con rol.
	AccionInfo Accion = "info"
	// AccionListar: un listado. Cualquiera con rol; el handler enseña a un
	// inquilino solo lo suyo.
	AccionListar Accion = "list"
	// AccionCrear: run, sandbox o graph up. El daemon sella el dueño y revisa
	// el cuerpo (plantilla visible, sin volúmenes ni carpetas del host).
	AccionCrear Accion = "create"
	// AccionImagenes: GET /images. Las imágenes base son de todos.
	AccionImagenes Accion = "images.list"
	// AccionMaquina: todo lo que actúa sobre la máquina {ref}: tiene que ser
	// del inquilino.
	AccionMaquina Accion = "machine"
	// AccionSnapLeer: GET /snapshots/{name}: suyo o compartido.
	AccionSnapLeer Accion = "snapshot.read"
	// AccionSnapEscribir: anotar, credenciales y borrar {name}: solo suyo.
	AccionSnapEscribir Accion = "snapshot.write"
	// AccionGrafo: todo lo que actúa sobre el grafo {ref}: tiene que ser suyo.
	AccionGrafo Accion = "graph"
	// AccionAdmin: solo admin (imágenes, volúmenes, store, carpetas del host,
	// métricas del host).
	AccionAdmin Accion = "admin"
)

// ruta es una ruta del daemon con su acción. revisar, si está, ve el cuerpo
// de un inquilino (ya leído) y devuelve el que llega al handler.
type ruta struct {
	patron  string
	accion  Accion
	revisar func(s *Server, t string, cuerpo []byte) ([]byte, int, error)
	h       http.HandlerFunc
}

// errAuthz es un error de autorización con su código HTTP.
func errAuthz(code int, format string, a ...any) error {
	return &api.StatusError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// autorizar envuelve el handler de rt con la decisión de la política.
func (s *Server) autorizar(rt ruta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authz == nil {
			rt.h(w, r)
			return
		}
		rol, err := s.rolDePeticion(r)
		// GET /info contesta también a quien no tiene rol (sin contar nada
		// suyo): es lo que deja a `kling doctor` decir por qué se le niega
		// todo lo demás. Un token inválido es un error también ahí.
		if err != nil && (rt.accion != AccionInfo || statusDe(err) == http.StatusUnauthorized) {
			fail(w, http.StatusForbidden, err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), claveRol{}, rol))
		if rol.Admin {
			rt.h(w, r)
			return
		}
		if err := s.permitir(r, rt, rol.Tenant); err != nil {
			fail(w, http.StatusForbidden, err)
			return
		}
		if rt.revisar != nil && r.Body != nil && r.Body != http.NoBody {
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, authzMaxFichero))
			if err != nil {
				fail(w, jsonBodyStatus(err), err)
				return
			}
			if len(b) > 0 {
				nb, code, err := rt.revisar(s, rol.Tenant, b)
				if err != nil {
					fail(w, code, err)
					return
				}
				b = nb
			}
			// El handler decodifica lo que se revisó y nada más: el cuerpo se
			// vuelve a codificar desde la estructura, así que no hay dos
			// lecturas distintas del mismo JSON (claves repetidas, campos
			// desconocidos) entre la comprobación y el uso.
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
		}
		rt.h(w, r)
	}
}

// rolDePeticion es el rol de quien hace r: el del token si trae uno, o el de
// su usuario del sistema.
func (s *Server) rolDePeticion(r *http.Request) (Rol, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			return Rol{}, errAuthz(http.StatusUnauthorized, "the Authorization header must be \"Bearer <token>\"")
		}
		rol, ok := s.authz.rolDeToken(tok)
		if !ok {
			return Rol{}, errAuthz(http.StatusUnauthorized, "the authz token is not valid (%s)", api.AuthzTokenEnv)
		}
		return rol, nil
	}
	ll := llamanteDe(r.Context())
	if !ll.Conocido {
		return Rol{}, errAuthz(http.StatusForbidden, "the daemon has an authz policy and couldn't tell who you are on its socket")
	}
	rol := s.authz.rol(ll)
	if !rol.Valido() {
		return Rol{}, errAuthz(http.StatusForbidden, "uid %d has no role in the daemon's authz policy (%s)", ll.UID, s.authz.Ruta)
	}
	return rol, nil
}

// permitir decide si el inquilino t puede hacer rt sobre el recurso de r. A
// lo que no es suyo responde como a lo que no existe: un inquilino no debe
// poder sondear nombres ajenos. Cuando resuelve una máquina o un grafo, fija
// en r su ID exacto, para que el handler actúe sobre lo que se comprobó y no
// sobre lo que el nombre resuelva un instante después.
func (s *Server) permitir(r *http.Request, rt ruta, t string) error {
	if t == "" && rt.accion != AccionInfo {
		return errAuthz(http.StatusForbidden, "no role for %s", rt.patron)
	}
	switch rt.accion {
	case AccionInfo, AccionListar, AccionCrear, AccionImagenes:
		return nil
	case AccionMaquina:
		ref := r.PathValue("ref")
		mc, ok := s.mgr.Get(ref)
		if !ok || !esDe(mc.Labels, t) {
			return errAuthz(http.StatusNotFound, "machine %q doesn't exist", ref)
		}
		r.SetPathValue("ref", mc.ID)
		return nil
	case AccionSnapLeer, AccionSnapEscribir:
		name := r.PathValue("name")
		snap, err := s.mgr.Snapshot(name)
		if err != nil || !s.authz.snapVisible(name, snap, t) {
			return errAuthz(http.StatusNotFound, "snapshot %q does not exist", name)
		}
		if rt.accion == AccionSnapEscribir && snap.Labels[api.LabelOwner] != t {
			return errAuthz(http.StatusForbidden, "snapshot %q is a shared template: tenants can use it, not change it", name)
		}
		return nil
	case AccionGrafo:
		ref := r.PathValue("ref")
		g, err := s.mgr.Graph(ref)
		if err != nil || dueñoGrafo(g) != t {
			return errAuthz(http.StatusNotFound, "graph %q doesn't exist", ref)
		}
		r.SetPathValue("ref", g.ID)
		return nil
	case AccionAdmin:
		return errAuthz(http.StatusForbidden, "%s needs the admin role; you are tenant:%s", rt.patron, t)
	}
	// Una acción sin decidir es un error de programación: se niega.
	return errAuthz(http.StatusForbidden, "%s has no authz decision", rt.patron)
}

// dueñoGrafo es el dueño de un grafo: el kling.owner que llevan TODOS sus
// nodos (autorizar lo sella en cada uno en graph up, y fork y snapshot lo
// heredan). "" si falta en alguno o no coinciden.
func dueñoGrafo(g *api.Graph) string {
	d := ""
	for _, n := range g.Nodes {
		o := n.Labels[api.LabelOwner]
		if o == "" || (d != "" && o != d) {
			return ""
		}
		d = o
	}
	return d
}

// ── revisión de cuerpos de un inquilino ──────────────────────────────────────

// etiquetasDeInquilino: kling.owner es del daemon, y kling.db.owner, si se
// pone, es el propio inquilino (el dueño de una base de kling db queda así
// ligado al dueño real, y el proxy de credenciales, que exige el mismo
// kling.db.owner en copia y agente, no cruza inquilinos).
func etiquetasDeInquilino(t string, labels map[string]string) error {
	for k, v := range labels {
		switch {
		case k == api.LabelOwner:
			return errAuthz(http.StatusForbidden, "label %s is set by the daemon from who you are", api.LabelOwner)
		case k == api.LabelDBOwner && v != "" && v != t:
			return errAuthz(http.StatusForbidden, "label %s must be your tenant %q (kling db -owner %s)", api.LabelDBOwner, t, t)
		}
	}
	return nil
}

// revisarNacimiento aplica a una máquina que va a nacer para el inquilino t
// (run, sandbox, un nodo de grafo) las reglas comunes, y sella su dueño.
func (s *Server) revisarNacimiento(t string, labels *map[string]string, from string, volumenes, carpetas bool) error {
	if err := etiquetasDeInquilino(t, *labels); err != nil {
		return err
	}
	// Los volúmenes no tienen dueño todavía, y una carpeta del host es del
	// host: los dos quedan para admin.
	if volumenes {
		return errAuthz(http.StatusForbidden, "volumes are admin-only under the daemon's authz policy")
	}
	if carpetas {
		return errAuthz(http.StatusForbidden, "host folders (shares) are admin-only under the daemon's authz policy")
	}
	if *labels == nil {
		*labels = map[string]string{}
	}
	if from != "" {
		snap, err := s.mgr.Snapshot(from)
		if err != nil || !s.authz.snapVisible(from, snap, t) {
			return errAuthz(http.StatusNotFound, "snapshot %q does not exist", from)
		}
		// Una plantilla compartida puede traer el kling.db.owner de otro:
		// la instancia lo heredaría. Se liga al inquilino.
		if o := snap.Labels[api.LabelDBOwner]; o != "" && o != t {
			if _, ya := (*labels)[api.LabelDBOwner]; !ya {
				(*labels)[api.LabelDBOwner] = t
			}
		}
	}
	(*labels)[api.LabelOwner] = t
	return nil
}

// recodificar decodifica b en un T, lo pasa por f y lo vuelve a codificar.
func recodificar[T any](b []byte, f func(*T) error) ([]byte, int, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := f(&v); err != nil {
		code := http.StatusForbidden
		var se *api.StatusError
		if errors.As(err, &se) && se.Code != 0 {
			code = se.Code
		}
		return nil, code, err
	}
	nb, err := json.Marshal(&v)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return nb, 0, nil
}

func revisarRun(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.RunRequest) error {
		return s.revisarNacimiento(t, &req.Labels, req.From, len(req.Volumes) > 0 || req.Volume != "", len(req.Shares) > 0)
	})
}

func revisarSandbox(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.SandboxRequest) error {
		return s.revisarNacimiento(t, &req.Labels, req.From, len(req.Volumes) > 0, len(req.Shares) > 0)
	})
}

func revisarGrafo(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.GraphRequest) error {
		for nombre, n := range req.Graph.Nodes {
			if err := s.revisarNacimiento(t, &n.Labels, n.From, len(n.Volumes) > 0, len(n.Shares) > 0); err != nil {
				return errAuthz(statusDe(err), "node %s: %v", nombre, err)
			}
			req.Graph.Nodes[nombre] = n
		}
		return nil
	})
}

func revisarEtiquetas(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(labels *map[string]string) error {
		return etiquetasDeInquilino(t, *labels)
	})
}

func revisarFork(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.ForkRequest) error {
		if err := etiquetasDeInquilino(t, req.Labels); err != nil {
			return err
		}
		if req.Labels == nil {
			req.Labels = map[string]string{}
		}
		req.Labels[api.LabelOwner] = t
		return nil
	})
}

// revisarCommit: replace no puede pisar el snapshot de otro (ni una
// plantilla compartida).
func revisarCommit(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.CommitRequest) error {
		if !req.Replace {
			return nil
		}
		if snap, err := s.mgr.Snapshot(req.Name); err == nil && snap.Labels[api.LabelOwner] != t {
			return errAuthz(http.StatusForbidden, "snapshot %q is not yours to replace", req.Name)
		}
		return nil
	})
}

// revisarCredenciales: una credencial que lleva a otra máquina (el attach de
// kling db) solo puede llevar a una del mismo inquilino, y con su dueño. Vale
// igual para las de una máquina y las de una plantilla.
func revisarCredenciales(s *Server, t string, b []byte) ([]byte, int, error) {
	return recodificar(b, func(req *api.CredentialsRequest) error {
		for _, c := range req.Credentials {
			if c.UpstreamMachine != "" {
				if mc, ok := s.mgr.Get(c.UpstreamMachine); !ok || !esDe(mc.Labels, t) {
					return errAuthz(http.StatusNotFound, "machine %q doesn't exist", c.UpstreamMachine)
				}
			}
			if c.UpstreamOwner != "" && c.UpstreamOwner != t {
				return errAuthz(http.StatusForbidden, "upstream_owner must be your tenant %q", t)
			}
		}
		return nil
	})
}

func statusDe(err error) int {
	var se *api.StatusError
	if errors.As(err, &se) && se.Code != 0 {
		return se.Code
	}
	return http.StatusForbidden
}

// ── filtros de listados ──────────────────────────────────────────────────────

func filtrarMaquinas(r *http.Request, l []*api.Machine) []*api.Machine {
	t, filtra := inquilinoDe(r)
	if !filtra {
		return l
	}
	out := []*api.Machine{}
	for _, mc := range l {
		if esDe(mc.Labels, t) {
			out = append(out, mc)
		}
	}
	return out
}

func (s *Server) filtrarSnapshots(r *http.Request, l []*api.Snapshot) []*api.Snapshot {
	t, filtra := inquilinoDe(r)
	if !filtra {
		return l
	}
	out := []*api.Snapshot{}
	for _, sn := range l {
		if t != "" && s.authz.snapVisible(sn.Name, sn, t) {
			out = append(out, sn)
		}
	}
	return out
}

func filtrarGrafos(r *http.Request, l []*api.Graph) []*api.Graph {
	t, filtra := inquilinoDe(r)
	if !filtra {
		return l
	}
	out := []*api.Graph{}
	for _, g := range l {
		if t != "" && dueñoGrafo(g) == t {
			out = append(out, g)
		}
	}
	return out
}

// eventoVisible dice si r puede ver ev: un inquilino, solo los de sus
// máquinas vivas (el de una ya borrada no se puede atribuir y no se envía).
func (s *Server) eventoVisible(r *http.Request, ev api.Event) bool {
	t, filtra := inquilinoDe(r)
	if !filtra {
		return true
	}
	if ev.ID == "" {
		return false
	}
	mc, ok := s.mgr.Get(ev.ID)
	return ok && mc.ID == ev.ID && esDe(mc.Labels, t)
}

// infoAuthz es lo que GET /info cuenta de la autorización a quien pregunta.
func (s *Server) infoAuthz(r *http.Request) *api.AuthzInfo {
	out := &api.AuthzInfo{Enabled: s.authz != nil}
	if ll := llamanteDe(r.Context()); ll.Conocido {
		uid := ll.UID
		out.UID = &uid
	}
	if rol, ok := rolDe(r); ok {
		out.Role = rol.String()
	}
	return out
}
