// Package mmds sirve el metadata service con la semántica V2 de Firecracker
// que usa pkg/guest/mmds.go del núcleo: token de sesión por PUT, lecturas con
// el token en cabecera y el árbol como JSON si se pide.
//
// Es el canal por el que el núcleo inyecta secretos de sesión en una máquina
// VIVA. El almacén vive solo en la memoria de este proceso: no viaja en el
// snapshot, igual que en Firecracker.
package mmds

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// MaxTokenTTL es el máximo de Firecracker (6 h).
	MaxTokenTTL = 21600
	// maxTokens acota los tokens vivos: el invitado es hostil y podría pedir
	// tokens en bucle para hacer crecer el mapa.
	maxTokens = 1024
)

// Store es el documento JSON del metadata service más sus tokens.
type Store struct {
	mu     sync.Mutex
	data   any
	tokens map[string]time.Time
	now    func() time.Time
}

func NewStore() *Store {
	return &Store{data: map[string]any{}, tokens: map[string]time.Time{}, now: time.Now}
}

// Put sustituye el almacén entero (PUT /mmds pisa, no mezcla).
func (s *Store) Put(v any) {
	s.mu.Lock()
	s.data = v
	s.mu.Unlock()
}

// Patch aplica un JSON merge patch (RFC 7396), como PATCH /mmds de Firecracker.
func (s *Store) Patch(patch any) {
	s.mu.Lock()
	s.data = mergePatch(s.data, patch)
	s.mu.Unlock()
}

// Get devuelve una copia del almacén.
func (s *Store) Get() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.data)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(t, k)
			continue
		}
		t[k] = mergePatch(t[k], v)
	}
	return t
}

func (s *Store) newToken(ttl time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.tokens) >= maxTokens {
		for t, exp := range s.tokens {
			if now.After(exp) {
				delete(s.tokens, t)
			}
		}
		if len(s.tokens) >= maxTokens {
			return "", false
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	s.tokens[tok] = now.Add(ttl)
	return tok, true
}

func (s *Store) validToken(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if s.now().After(exp) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

// Handler es el servidor HTTP que ve el invitado en 169.254.169.254:80.
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

func (s *Store) serve(w http.ResponseWriter, r *http.Request) {
	// Firecracker rechaza lo que venga reenviado por un proxy: un token de MMDS
	// solo tiene sentido pedido desde el propio invitado.
	if r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "Invalid header. Reason: X-Forwarded-For is not allowed.", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.serveToken(w, r)
	case http.MethodGet:
		s.serveGet(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "Not allowed HTTP method.", http.StatusMethodNotAllowed)
	}
}

func (s *Store) serveToken(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSuffix(r.URL.Path, "/") != "/latest/api/token" {
		http.Error(w, "Not allowed HTTP method.", http.StatusMethodNotAllowed)
		return
	}
	v := r.Header.Get("X-metadata-token-ttl-seconds")
	if v == "" {
		v = r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds")
	}
	ttl, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || ttl < 1 || ttl > MaxTokenTTL {
		http.Error(w, "Invalid time to live value provided for token: "+v+
			". Please provide a value between 1 and 21600.", http.StatusBadRequest)
		return
	}
	tok, ok := s.newToken(time.Duration(ttl) * time.Second)
	if !ok {
		http.Error(w, "Token generation failed.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(tok))
}

func (s *Store) serveGet(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get("X-metadata-token")
	if tok == "" {
		tok = r.Header.Get("X-aws-ec2-metadata-token")
	}
	if !s.validToken(tok) {
		http.Error(w, "No MMDS token provided. Use `X-metadata-token` header to specify the session token.", http.StatusUnauthorized)
		return
	}
	node, ok := lookup(s.Get(), r.URL.Path)
	if !ok {
		http.Error(w, "Resource not found: "+r.URL.Path+".", http.StatusNotFound)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(node)
		return
	}
	// Formato IMDS: los objetos listan sus claves (las que son objetos con "/"),
	// las cadenas van en crudo y el resto no tiene representación.
	switch v := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k, c := range v {
			if _, isObj := c.(map[string]any); isObj {
				k += "/"
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Join(keys, "\n")))
	case string:
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(v))
	default:
		http.Error(w, "Cannot retrieve value. The value has an unsupported type.", http.StatusBadRequest)
	}
}

func lookup(root any, path string) (any, bool) {
	node := root
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[part]; !ok {
			return nil, false
		}
	}
	return node, true
}
