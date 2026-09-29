package main

// Autenticación de la API del teléfono (issue #110, docs/phoned.md).
//
// El 8091 no sabe quién le habla: el proxy del daemon (POST
// /machines/{ref}/guest) y una arista de grafo (link) llegan los dos desde el
// anfitrión, por la misma dirección, así que no hay forma de distinguirlos por
// el origen. Por eso TODA ruta salvo /v1/health (y el índice) exige un token
// de portador:
//
//	Authorization: Bearer <token>
//
// El invitado no guarda tokens, solo sus sha256, con un ámbito:
//
//	read     GET /v1/screen, GET /v1/tree (ver, no tocar)
//	control  todo lo demás (tocar, escribir, instalar, launch, logs, identity,
//	         verify-cache) y también lo de read
//
// Llegan con la identidad del clon por MMDS (identity.go, "api_tokens") y se
// escriben en la RAM de la VM (/run/kindling-android/api-tokens.json, 0600):
// viajan con la memoria en pause y freeze, un dorado no tiene ninguno (nadie
// le dio identidad) y un clon recién restaurado tampoco hasta su gancho. Sin
// tokens la API está CERRADA: solo contesta /v1/health. Es a propósito: un
// nodo de grafo hecho del dorado, sin identidad, no se puede controlar por
// una arista.
//
// El token de control de cada clon lo genera quien le da la identidad (kling
// phone lo guarda en el store del daemon, que solo se lee con acceso al
// socket: la misma confianza que el propio proxy). Dar el control a un nodo
// de grafo es darle ese token; sin él, una arista al 8091 recibe 401.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// tokensPath: en la RAM de la VM, junto a la marca de identidad.
const tokensPath = stateDir + "/api-tokens.json"

// Ámbitos.
const (
	scopeNone    = ""        // abierto: /v1/health e índice
	scopeRead    = "read"    // ver
	scopeControl = "control" // todo
)

// maxAPITokens: los de kling phone son uno o dos; esto solo acota el fichero.
const maxAPITokens = 16

// apiToken es un token aceptado: su sha256 (hex) y su ámbito.
type apiToken struct {
	SHA256 string `json:"sha256"`
	Scope  string `json:"scope"`
}

var reSHA256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validateTokens comprueba una lista de tokens tal como llega por MMDS.
func validateTokens(ts []apiToken) error {
	if len(ts) > maxAPITokens {
		return fmt.Errorf("phone.api_tokens: at most %d tokens", maxAPITokens)
	}
	for i, t := range ts {
		if !reSHA256Hex.MatchString(t.SHA256) {
			return fmt.Errorf("phone.api_tokens[%d].sha256 must be 64 lowercase hex digits (the sha256 of the token, never the token)", i)
		}
		switch t.Scope {
		case scopeRead, scopeControl:
		default:
			return fmt.Errorf("phone.api_tokens[%d].scope must be read or control", i)
		}
	}
	return nil
}

// writeTokens deja la lista (validada) en tokensPath de forma atómica.
func writeTokens(path string, ts []apiToken) error {
	if ts == nil {
		ts = []apiToken{}
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// requiredScope es el ámbito que pide una ruta.
func requiredScope(method, path string) string {
	switch {
	case method == http.MethodGet && (path == "/v1/health" || path == "/"):
		return scopeNone
	case method == http.MethodGet && (path == "/v1/screen" || path == "/v1/tree"):
		return scopeRead
	}
	return scopeControl
}

// authz lee los tokens aceptados del fichero (cacheado por mtime y tamaño:
// el gancho de identidad, que es otro proceso, lo reescribe).
type authz struct {
	path string

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	loaded bool
	tokens []apiToken
}

func newAuthz(path string) *authz { return &authz{path: path} }

// current devuelve los tokens aceptados ahora (vacío si no hay fichero o no se
// entiende: cerrado).
func (a *authz) current() []apiToken {
	st, err := os.Stat(a.path)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.loaded, a.tokens = false, nil
		return nil
	}
	if a.loaded && st.ModTime().Equal(a.mtime) && st.Size() == a.size {
		return a.tokens
	}
	a.loaded, a.tokens = true, nil
	a.mtime, a.size = st.ModTime(), st.Size()
	b, err := os.ReadFile(a.path)
	if err != nil {
		return nil
	}
	var ts []apiToken
	if json.Unmarshal(b, &ts) != nil || validateTokens(ts) != nil {
		return nil
	}
	a.tokens = ts
	return ts
}

// check dice qué ámbito da el token presentado (scopeNone si ninguno).
func (a *authz) check(presented string) string {
	if presented == "" {
		return scopeNone
	}
	sum := sha256.Sum256([]byte(presented))
	got := hex.EncodeToString(sum[:])
	best := scopeNone
	// Se recorren todos, sin salir antes: el tiempo no dice cuál casó.
	for _, t := range a.current() {
		if subtle.ConstantTimeCompare([]byte(got), []byte(t.SHA256)) == 1 {
			if t.Scope == scopeControl || best == scopeNone {
				best = t.Scope
			}
		}
	}
	return best
}

// bearer saca el token de la cabecera Authorization.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// wrap protege h: 401 sin token válido, 403 con uno de ámbito insuficiente.
func (a *authz) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := requiredScope(r.Method, r.URL.Path)
		if need == scopeNone {
			h.ServeHTTP(w, r)
			return
		}
		got := a.check(bearer(r))
		switch {
		case got == scopeNone:
			w.Header().Set("WWW-Authenticate", `Bearer realm="kling-phoned"`)
			msg := "missing or invalid API token (Authorization: Bearer ...; docs/phoned.md)"
			if len(a.current()) == 0 {
				msg = "the phone API is locked: this phone has no API token yet (its identity was not applied)"
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": msg})
			return
		case need == scopeControl && got != scopeControl:
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "this token can only read (screen, tree)"})
			return
		}
		h.ServeHTTP(w, r)
	})
}
