package main

import (
	"io"
	"strings"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// extension declara lo que kindling-sandbox añade a `kling`.
//
// El comando es `sbx` y no `sandbox` a propósito: `kling sandbox` es del núcleo
// —crea una microVM de usar y tirar contra el daemon local— y los comandos del
// núcleo ganan siempre. Esto es otra cosa: habla con un FRONTAL, que a su vez
// reparte entre varios daemons, con plantillas, inquilinos y cuotas. Por eso
// `sbx` se promueve a primer nivel (TopLevel): bajo el nombre de la extensión
// sería `kling sandbox ...`, que es del núcleo.
func extension() *plugin.Builtin {
	return &plugin.Builtin{
		Manifest: plugin.Manifest{
			ManifestVersion: plugin.ManifestVersion,
			Name:            "sandbox",
			Version:         strings.TrimPrefix(Version, "v"),
			// 0.17.0 por los grafos (GraphUp/Freeze/Thaw/Remove y las aristas de
			// credencial): es la API más nueva del núcleo que usa el frontal.
			// Con un núcleo anterior los comandos se instalaban y fallaban con
			// 404 en vez de con un aviso de versión al instalar.
			MinKling:  "0.17.0",
			Summary:   "sandboxes for code agents: templates, tenants and several hosts",
			HelpGroup: "SERVE",
			Commands: []plugin.Command{{
				Name:     "sbx",
				TopLevel: true,
				Group:    "SANDBOX FLEET",
				Summary:  "sandboxes served by a gateway (templates, tenants, several hosts)",
				Usage: "  sbx gateway [-listen ADDR] [-tls-cert/-tls-key]  serves the sandbox API\n" +
					"  sbx template apply -f tpl.json | ls | rebuild N  recipes that become golden snapshots\n" +
					"  sbx new -template T [-ttl 10m] | ls | rm ID      sandboxes through the gateway\n" +
					"  sbx exec ID -- cmd... | shell ID | cp ...        work inside one\n" +
					"  sbx hosts                                        which daemons are behind the gateway",
				Subcommands: []string{"gateway", "template", "new", "ls", "rm", "renew", "exec", "shell", "cp", "hosts"},
			}},
			Config: []plugin.ConfigKey{
				{Key: "listen", Type: "string", Help: "address the gateway listens on (default 127.0.0.1:8090; off loopback it needs TLS or -insecure-listen)"},
				{Key: "tls_cert", Type: "string", Help: "TLS certificate (PEM) the gateway serves HTTPS with"},
				{Key: "tls_key", Type: "string", Help: "private key (PEM) of sandbox.tls_cert"},
				{Key: "url", Type: "string", Help: "gateway the client commands talk to (default http://127.0.0.1:8090)"},
				{Key: "token", Type: "secret", Help: "token the client commands send"},
				{Key: "hosts", Type: "string", Help: "kindling daemons behind the gateway: name=endpoint, comma separated"},
				{Key: "idle", Type: "string", Help: "how long a sandbox may sit unused before it is frozen (default 5m)"},
				{Key: "reap", Type: "string", Help: "how long a frozen sandbox is kept before it is destroyed (default 24h)"},
			},
			Units: []string{"kling-sandbox.service"},
			Hooks: []string{plugin.HookStatus},
		},
		Commands: map[string]func([]string) error{
			"sbx": cmdSbx,
		},
		Hooks: map[string]func([]string, io.Writer) error{
			plugin.HookStatus: hookStatus,
		},
	}
}
