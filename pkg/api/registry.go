package api

import (
	"context"
	"net/http"
	"net/url"
)

// CapabilityRegistryAuth es la capacidad de GET /info que dice que el daemon
// guarda credenciales de registros privados (/registries) y las usa al
// importar imágenes OCI.
const CapabilityRegistryAuth = "registry-auth"

// RegistryLoginRequest es el cuerpo de POST /registries: las credenciales de
// un registro, que el daemon guarda (0600, de root) y solo usa con él.
type RegistryLoginRequest struct {
	// Host es el registro: "ghcr.io", "localhost:5000", "docker.io".
	Host     string `json:"host"`
	Username string `json:"username,omitempty"`
	// Password es la contraseña o el token. No vuelve nunca por el API.
	Password string `json:"password"`
}

// Registry es un registro con credenciales guardadas, sin la contraseña.
type Registry struct {
	Host     string `json:"host"`
	Username string `json:"username,omitempty"`
}

// Registries lista los registros con credenciales (sin ellas).
func (c *Client) Registries(ctx context.Context) ([]Registry, error) {
	var l []Registry
	return l, c.do(ctx, http.MethodGet, "/registries", nil, &l)
}

// RegistryLogin guarda (o sustituye) las credenciales de un registro.
func (c *Client) RegistryLogin(ctx context.Context, r RegistryLoginRequest) (*Registry, error) {
	var out Registry
	return &out, c.do(ctx, http.MethodPost, "/registries", r, &out)
}

// RegistryLogout borra las credenciales de un registro.
func (c *Client) RegistryLogout(ctx context.Context, host string) error {
	return c.do(ctx, http.MethodDelete, "/registries/"+url.PathEscape(host), nil, nil)
}
