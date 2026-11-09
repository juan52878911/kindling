package net

import (
	"fmt"
	"slices"
	"strings"
)

// AVISO DE DOMINIOS MULTIINQUILINO EN -allow.
//
// egress allowlist filtra por IP: abre las direcciones que resuelven los
// dominios permitidos (dnsresolver.go en Linux, vz/internal/egress en macOS).
// Si el dominio vive en un CDN o en un hosting compartido, esas IPs sirven
// también a sitios de terceros, y el invitado llega a cualquiera de ellos
// presentando su nombre en el SNI o en el Host. Y permitir el propio sufijo
// (github.io, amazonaws.com) permite, por la regla de subdominios, cualquier
// sitio de cualquier inquilino. No se bloquea —hay usos legítimos—, se avisa
// (SECURITY.md, «Lo que NO está resuelto»; docs/egress-sni.md).
//
// No se usa la Public Suffix List entera (golang.org/x/net/publicsuffix no está
// entre las dependencias del núcleo y la sección privada de la PSL tampoco es
// exhaustiva): basta una lista corta de los hostings donde cualquiera publica
// un sitio, más los sufijos públicos que se escriben a mano por error (un TLD,
// co.uk). Lo que no está aquí no avisa; un dominio propio detrás de Cloudflare
// tampoco, porque eso solo se ve resolviendo.

// sufijosMultiinquilino son sufijos bajo los que cualquiera puede publicar un
// sitio o un servidor propio, en la infraestructura (y las IPs) del proveedor.
var sufijosMultiinquilino = []string{
	// Páginas y contenido de forjas.
	"github.io", "githubusercontent.com", "gitlab.io", "bitbucket.io", "codeberg.page",
	"sourceforge.io", "readthedocs.io",
	// AWS: S3, CloudFront, Elastic Beanstalk, Amplify, App Runner, Lambda URLs.
	"amazonaws.com", "cloudfront.net", "elasticbeanstalk.com", "amplifyapp.com",
	"awsapprunner.com", "on.aws",
	// Azure.
	"azurewebsites.net", "azureedge.net", "azurefd.net", "cloudapp.net",
	"blob.core.windows.net", "azurestaticapps.net", "trafficmanager.net",
	// Google Cloud y Firebase.
	"appspot.com", "run.app", "cloudfunctions.net", "firebaseapp.com", "web.app",
	"storage.googleapis.com",
	// Cloudflare.
	"pages.dev", "workers.dev", "r2.dev", "trycloudflare.com",
	// PaaS y hosting estático.
	"herokuapp.com", "vercel.app", "now.sh", "netlify.app", "fly.dev", "onrender.com",
	"railway.app", "deno.dev", "surge.sh", "glitch.me", "replit.app", "repl.co",
	"ondigitalocean.app", "digitaloceanspaces.com", "pythonanywhere.com",
	"blogspot.com", "wordpress.com",
	// CDN genéricos (nombres del borde, no de un cliente).
	"fastly.net", "akamaized.net", "edgekey.net", "edgesuite.net", "b-cdn.net",
	// Túneles públicos.
	"ngrok.io", "ngrok.app", "ngrok-free.app", "loca.lt",
}

// sufijosPublicos son sufijos públicos (de registro) de dos etiquetas que se
// cuelan en una lista a mano. Un dominio de una sola etiqueta (un TLD) avisa
// siempre, esté o no aquí.
var sufijosPublicos = []string{
	"co.uk", "org.uk", "ac.uk", "gov.uk", "com.au", "net.au", "org.au", "co.nz",
	"com.br", "net.br", "com.mx", "com.ar", "com.co", "com.es", "com.pe", "com.cn",
	"com.tw", "com.hk", "co.jp", "ne.jp", "or.jp", "co.kr", "co.in", "co.za",
}

// AvisosAllowMultiinquilino devuelve un aviso, en inglés y listo para la CLI o
// el log, por cada dominio de -allow que sea un sufijo público o multiinquilino
// o que cuelgue de uno. Vacío si no hay nada que decir.
func AvisosAllowMultiinquilino(domains []string) []string {
	var out []string
	vistos := map[string]bool{}
	for _, d := range normalizeDomains(domains) {
		d = strings.TrimPrefix(d, "*.")
		if d == "" || vistos[d] {
			continue
		}
		vistos[d] = true
		if a := avisoDominio(d); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func avisoDominio(d string) string {
	if !strings.Contains(d, ".") || slices.Contains(sufijosPublicos, d) {
		return fmt.Sprintf("-allow %s: %q is a public suffix; allowing it allows every domain registered under it", d, d)
	}
	for _, s := range sufijosMultiinquilino {
		if d == s {
			return fmt.Sprintf("-allow %s: %q is a shared hosting suffix; anyone can publish a site under it, "+
				"so the guest can reach any of them. Allow the exact host you need instead", d, d)
		}
		if strings.HasSuffix(d, "."+s) {
			return fmt.Sprintf("-allow %s: it is served from shared infrastructure (%s); the allowlist filters by IP, "+
				"so the guest can also reach other sites on the same addresses. See SECURITY.md", d, s)
		}
	}
	return ""
}
