// Autenticación del gateway.
//
// El daemon se protege no escuchando en la red. El gateway sí escucha, así que
// necesita otra cosa: despertar un snapshot para llamar a una de sus
// herramientas ES ejecutar código, y no puede quedar al alcance de cualquiera
// que llegue al puerto. Un token basta, y no arrastra dependencias.
package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// bearerPrefix es el esquema de la cabecera. El RFC 7235 lo declara
// insensible a mayúsculas, y hay clientes que mandan "bearer".
const bearerPrefix = "Bearer "

// NewToken genera un token nuevo.
//
// base64url sin relleno: sobrevive a pegarlo en un JSON, en una cabecera o en
// una URL sin que nadie tenga que escapar nada.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("could not generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Auth exige `Authorization: Bearer <token>` en todo menos en /healthz.
//
// SOLO cabecera, nunca `?token=`: un secreto en la query acaba en el registro
// de cualquier proxy intermedio y en el historial del cliente. Los clientes MCP
// que importan admiten cabeceras sobre transporte HTTP, así que no hay excusa
// para la comodidad insegura.
//
// Un token vacío devuelve el handler sin envolver. Que eso sea seguro depende
// de quien llama, y por eso `kling gateway` se niega a hacerlo fuera de
// loopback: ver IsLoopback.
//
// Delega en authHandler: el token único es el tenant "default" sin cuotas. La
// resolución token->tenant y las cuotas viven en quota.go; aquí se conserva la
// firma sencilla que usan quienes solo tienen un token.
func Auth(h http.Handler, token string) http.Handler {
	g := &Gateway{}
	return g.authHandler(h, token)
}

// bearer extrae el token de una cabecera Authorization.
func bearer(header string) (string, bool) {
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return header[len(bearerPrefix):], true
}
