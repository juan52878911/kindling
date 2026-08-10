// Package registry consulta el registro oficial de servidores MCP.
//
// El oficial —registry.modelcontextprotocol.io— y no mcp.so, glama o mcp.run:
// es el que publican los propios servidores como fuente de verdad, y su esquema
// está versionado. Los agregadores de terceros van por detrás y cada uno
// inventa su formato.
//
// Sin dependencias, como el resto del proyecto: son dos endpoints GET y un
// esquema del que solo interesa un tercio.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BaseURL es el registro oficial.
const BaseURL = "https://registry.modelcontextprotocol.io"

// cacheTTL: el catálogo cambia despacio y la latencia se nota en cada búsqueda.
const cacheTTL = 6 * time.Hour

// Server es una entrada del registro, con solo lo que kindling sabe usar.
type Server struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	Repository  Repo      `json:"repository"`
	Packages    []Package `json:"packages"`
	Remotes     []Remote  `json:"remotes"`
}

type Repo struct {
	URL string `json:"url"`
}

// Remote es un servidor que YA está alojado por otro. kindling no lo empaqueta:
// no tiene sentido meter en una microVM algo que corre en la nube de un tercero.
type Remote struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type Package struct {
	RegistryType string `json:"registryType"` // npm | pypi | oci | nuget | mcpb
	Identifier   string `json:"identifier"`
	Version      string `json:"version"`
	RuntimeHint  string `json:"runtimeHint"` // npx | docker | dnx | ""
	Transport    struct {
		Type string `json:"type"` // stdio | sse | streamable-http
	} `json:"transport"`
	RuntimeArguments []Argument `json:"runtimeArguments"`
	PackageArguments []Argument `json:"packageArguments"`
	EnvironmentVars  []EnvVar   `json:"environmentVariables"`
}

// Argument es un argumento de la línea de comandos del servidor.
type Argument struct {
	Type        string `json:"type"` // positional | named
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description"`
	Format      string `json:"format"`
	ValueHint   string `json:"valueHint"`
	IsRequired  bool   `json:"isRequired"`
	IsRepeated  bool   `json:"isRepeated"`
}

type EnvVar struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     string `json:"default"`
	IsRequired  bool   `json:"isRequired"`
	IsSecret    bool   `json:"isSecret"`
}

// respuesta del endpoint /v0/servers.
type listResponse struct {
	Servers []struct {
		Server Server `json:"server"`
	} `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
	} `json:"metadata"`
}

// Client consulta el registro con caché en disco.
type Client struct {
	Base  string
	Dir   string // caché; vacío la desactiva
	HTTP  *http.Client
	Fresh bool // ignorar la caché aunque esté vigente
}

func New() *Client {
	return &Client{
		Base: BaseURL,
		Dir:  cacheDir(),
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

func cacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "kindling", "registry")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "kindling", "registry")
}

// Search busca por texto y devuelve solo la última versión de cada servidor.
//
// Sin `version=latest` el registro devuelve TODAS las versiones publicadas, así
// que una búsqueda de "filesystem" saldría con el mismo servidor cinco veces.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Server, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{
		"search":  {query},
		"version": {"latest"},
		"limit":   {fmt.Sprint(limit)},
	}
	var out listResponse
	if err := c.get(ctx, "/v0/servers?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	servers := make([]Server, 0, len(out.Servers))
	for _, e := range out.Servers {
		servers = append(servers, e.Server)
	}
	return servers, nil
}

// Get resuelve un servidor por nombre.
//
// Admite el nombre completo con espacio de nombres inverso
// ("io.github.usuario/servidor") y también el trozo final, que es lo que la
// gente teclea: `kling add filesystem`. En ese caso se busca y se exige que la
// coincidencia sea inequívoca, porque instalar el servidor equivocado de otro
// autor es un fallo silencioso y desagradable.
func (c *Client) Get(ctx context.Context, name string, limit int) (*Server, []Server, error) {
	servers, err := c.Search(ctx, name, limit)
	if err != nil {
		return nil, nil, err
	}
	if len(servers) == 0 {
		return nil, nil, fmt.Errorf("no encuentro %q en el registro oficial", name)
	}

	var exact, suffix []Server
	for _, s := range servers {
		if s.Name == name {
			exact = append(exact, s)
			continue
		}
		if _, last, ok := strings.Cut(s.Name, "/"); ok && last == name {
			suffix = append(suffix, s)
		}
	}
	switch {
	case len(exact) == 1:
		return &exact[0], nil, nil
	case len(suffix) == 1:
		return &suffix[0], nil, nil
	case len(exact) > 1:
		return nil, exact, fmt.Errorf("%q está publicado por varios; usa el nombre completo", name)
	case len(suffix) > 1:
		return nil, suffix, fmt.Errorf("%q es ambiguo; usa el nombre completo", name)
	}
	return nil, servers, fmt.Errorf("no hay ninguno que se llame exactamente %q", name)
}

// Stdio devuelve el paquete que kindling sabe empaquetar.
//
// Se queda con stdio a propósito. El puente stdio->HTTP ya resuelve ese caso y
// es el mayoritario; un paquete que solo habla SSE o streamable-http tendría que
// escuchar dentro del invitado con una configuración que el registro no
// describe, así que fingir que lo soportamos daría un servicio que no arranca.
func (s *Server) Stdio() (Package, bool) {
	for _, p := range s.Packages {
		if p.Transport.Type == "stdio" && p.RegistryType == "npm" {
			return p, true
		}
	}
	return Package{}, false
}

// MissingEnv lista las variables obligatorias que no se han aportado.
func (p Package) MissingEnv(given map[string]string) []EnvVar {
	var missing []EnvVar
	for _, v := range p.EnvironmentVars {
		if !v.IsRequired || v.Default != "" {
			continue
		}
		if _, ok := given[v.Name]; !ok {
			missing = append(missing, v)
		}
	}
	return missing
}

// get pide una ruta al registro, con caché en disco.
func (c *Client) get(ctx context.Context, path string, out any) error {
	if b, ok := c.fromCache(path); ok {
		if err := json.Unmarshal(b, out); err == nil {
			return nil
		}
		// Una caché ilegible se ignora y se vuelve a pedir: no es un error del
		// que haya que enterar a nadie.
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("no alcanzo el registro: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("el registro respondió %s", resp.Status)
	}

	// Se lee entero para poder guardarlo en caché y decodificarlo del mismo
	// buffer. Son cientos de KB en el peor caso; no compensa complicarlo.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("el registro devolvió algo que no entiendo: %w", err)
	}
	c.store(path, b)
	return nil
}

func (c *Client) cachePath(path string) string {
	if c.Dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:8])+".json")
}

func (c *Client) fromCache(path string) ([]byte, bool) {
	if c.Fresh {
		return nil, false
	}
	p := c.cachePath(path)
	if p == "" {
		return nil, false
	}
	fi, err := os.Stat(p)
	if err != nil || time.Since(fi.ModTime()) > cacheTTL {
		return nil, false
	}
	b, err := os.ReadFile(p)
	return b, err == nil
}

func (c *Client) store(path string, b []byte) {
	p := c.cachePath(path)
	if p == "" {
		return
	}
	// La caché es una optimización: si no se puede escribir, no pasa nada y no
	// hay que molestar a nadie con el aviso.
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}
