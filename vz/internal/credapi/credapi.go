// Package credapi es el cuerpo de PUT /kling/credentials, el mismo en los dos
// procesos que pueden recibir las claves de una máquina: el VMM (kling-vz o
// kling-hv, el modelo de antes) y kling-credproxy (el proxy aparte, ver
// docs/proxy-macos-separado.md del núcleo). Lo que manda el daemon es
// internal/fc.KlingCredential.
package credapi

import "github.com/juan52878911/kindling/pkg/credproxy"

// Credencial es una credencial tal como viaja por el socket.
type Credencial struct {
	Env         string   `json:"env"`
	Domain      string   `json:"domain"`
	Placeholder string   `json:"placeholder"`
	Secret      string   `json:"secret"`
	Allow       []string `json:"allow,omitempty"`
	Headers     []string `json:"headers,omitempty"`
	Query       bool     `json:"query,omitempty"`
	Body        bool     `json:"body,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Port        int      `json:"port,omitempty"`
	User        string   `json:"user,omitempty"`
	Database    string   `json:"database,omitempty"`
	AnyDatabase bool     `json:"any_database,omitempty"`
	CAPEM       string   `json:"ca_pem,omitempty"`
	// Upstream fijado por el operador (ver pkg/credproxy/upstream.go): el
	// proxy marca desde la pila de red del Mac, así que 127.0.0.1 es el
	// loopback del Mac, donde Docker Desktop publica.
	Upstream      string `json:"upstream,omitempty"`
	UpstreamTLS   string `json:"upstream_tls,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`
	// Otra máquina de kindling (arista credential, kling db attach): la
	// conexión se pide al daemon en cada sesión (vz/internal/grafo).
	UpstreamMachine string `json:"upstream_machine,omitempty"`
	UpstreamOwner   string `json:"upstream_owner,omitempty"`
	// S3 y compatibles (Kind "sigv4"): el proxy firma de nuevo con
	// AccessKeyID y Secret (ver pkg/credproxy/sigv4.go).
	AccessKeyID string `json:"access_key_id,omitempty"`
	Region      string `json:"region,omitempty"`
	Service     string `json:"service,omitempty"`
}

// Cuerpo es el de PUT /kling/credentials.
type Cuerpo struct {
	Credentials []Credencial `json:"credentials"`
}

// Credential es c para el proxy.
func (c Credencial) Credential() credproxy.Credential {
	return credproxy.Credential{
		Env: c.Env, Domain: c.Domain, Placeholder: c.Placeholder, Secret: c.Secret,
		Allow:   c.Allow,
		Headers: c.Headers, Query: c.Query, Body: c.Body,
		Kind: c.Kind, Port: c.Port, User: c.User, Database: c.Database, AnyDatabase: c.AnyDatabase, CAPEM: c.CAPEM,
		Upstream: c.Upstream, UpstreamTLS: c.UpstreamTLS, TLSServerName: c.TLSServerName,
		UpstreamMachine: c.UpstreamMachine, UpstreamOwner: c.UpstreamOwner,
		AccessKeyID: c.AccessKeyID, Region: c.Region, Service: c.Service,
	}
}

// Kinds son los tipos de credencial que entienden el proxy de un VMM y el
// proxy aparte (credential_kinds de GET /kling/info). El daemon lo pregunta
// antes de mandar una credencial Postgres o MySQL: uno anterior ignoraría el
// tipo y la serviría como HTTP. "postgres-upstream" dice que entiende además
// Upstream, UpstreamTLS y TLSServerName (también en las MySQL, que nacieron
// con ellos). "sigv4" dice que lee AccessKeyID, Region y Service y firma de
// nuevo en vez de cambiar el marcador.
var Kinds = []string{credproxy.KindHTTP, credproxy.KindPostgres, credproxy.CapPostgresUpstream, credproxy.KindMySQL, credproxy.CapHTTPPlaces, credproxy.KindSigV4}
