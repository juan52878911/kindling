package oci

import (
	"encoding/base64"
	"fmt"
	"net"
	"strings"
)

// REGISTROS PRIVADOS. Sin credenciales, el cliente pide tokens anónimos de
// lectura, como siempre. Con Client.Auth, las de un registro se usan solo con
// él:
//
//   - Basic directo, si el registro lo pide (un 401 con "Basic"): registry:2
//     con htpasswd, ECR;
//   - el flujo Bearer: el token se pide al servicio que indica el 401 (realm)
//     con Basic. El realm tiene que ser https (o de esta máquina, para un
//     registro de pruebas) y estar en el dominio del registro: el de Docker Hub
//     es auth.docker.io para registry-1.docker.io, el de GitLab gitlab.com
//     para registry.gitlab.com. A un realm de otro dominio no se le mandan.
//
// Nunca viajan tras una redirección a otro host (las capas suelen redirigir a
// un CDN con la URL ya firmada): checkRedirect quita la cabecera. Y nunca
// salen en un error ni en el log.

// Credential es el usuario y la contraseña (o el token) de un registro.
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// basic es la cabecera Authorization de la credencial.
func (c Credential) basic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password))
}

// CredentialKey es la clave con la que se guardan las credenciales de un
// registro: el host en minúsculas, con su puerto, sin esquema ni ruta (lo que
// escribe la gente y lo que hay en ~/.docker/config.json:
// "https://index.docker.io/v1/"). Los nombres de Docker Hub son todos
// "docker.io".
func CredentialKey(host string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return "docker.io", nil
	}
	if h == "" || len(h) > 253 || !reHost.MatchString(h) {
		return "", fmt.Errorf("invalid registry host %q (want host[:port], e.g. ghcr.io or localhost:5000)", host)
	}
	return h, nil
}

// credFor son las credenciales de registry (nil si no hay).
func (c *Client) credFor(registry string) *Credential {
	if len(c.Auth) == 0 {
		return nil
	}
	k, err := CredentialKey(registry)
	if err != nil {
		return nil
	}
	if cr, ok := c.Auth[k]; ok {
		return &cr
	}
	return nil
}

// realmPermitido dice si se le pueden mandar las credenciales de registry al
// servicio de tokens realmHost: el mismo host, o el dominio del registro sin
// su primera etiqueta (registry-1.docker.io → docker.io, que cubre
// auth.docker.io) si quedan al menos dos. Con una IP o un registro de dos
// etiquetas (ghcr.io), solo él y sus subdominios. Los de esta máquina, entre
// ellos.
func realmPermitido(registry, realmHost string) bool {
	reg := strings.ToLower(registryHost(registry))
	realm := strings.ToLower(strings.TrimSuffix(realmHost, "."))
	if realm == reg || (isLocalHost(reg) && isLocalHost(realm)) {
		return true
	}
	if net.ParseIP(reg) != nil {
		return false
	}
	dom := reg
	if _, resto, ok := strings.Cut(reg, "."); ok && strings.Contains(resto, ".") {
		dom = resto
	}
	return realm == dom || strings.HasSuffix(realm, "."+dom)
}

// pista es lo que se añade a un 401 o un 403: con credenciales, que se
// rechazaron; sin ellas, cómo darlas. Nunca las credenciales.
func (c *Client) pista(registry string) string {
	k, err := CredentialKey(registry)
	if err != nil {
		k = registry
	}
	if c.credFor(registry) != nil {
		return fmt.Sprintf(" (the credentials saved for %s were refused or have no access to this repository; "+
			"update them with: kling registry login %s)", k, k)
	}
	return fmt.Sprintf(" (if the image is private, save the registry's credentials on the daemon: kling registry login %s)", k)
}
