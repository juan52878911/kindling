// Command kling-domotica es la extensión de kling que sirve `kling domotica`:
// las herramientas de la habitación de demo (decidir, evaluar y entrenar las
// capas de pkg/domotica) sin daemon, salvo eval-llm.
//
// Salió del núcleo en v0.13.0: son herramientas de una demo y el binario de
// quien solo usa sandboxes no las necesita. pkg/domotica sí sigue en el
// núcleo, porque el gateway de IA (pkg/aigw) lo usa para /v1/decide. Es además
// el ejemplo grande de extensión; examples/hello-extension es el mínimo.
//
//	kling plugins install domotica
//	go build -o ~/.local/share/kling/plugins/kling-domotica ./examples/domotica/cmd/kling-domotica
package main

import (
	"strings"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// Version se fija al compilar: -ldflags "-X main.Version=...".
var Version = "dev"

// domoticaSubcommands son los subcomandos que ofrece el completado; cmdDomotica
// (domotica.go) despacha exactamente estos.
var domoticaSubcommands = []string{"decide", "eval", "train-slots", "embed", "train-encoder", "templates", "eval-llm"}

const domoticaHelp = `  domotica decide [-lang L] "<text>"               smart-home decision: demo templates →
                                                   Chispa intent + slots, or escalate
  domotica eval -data t.jsonl [-challenge]         accuracy, slot F1, exact match, latency
  domotica train-slots -data d.jsonl -o m.chispas  trains the slot tagger (docs/domotica.md)
  domotica embed -url U -data a.jsonl -o c.jemb    caches sentence-encoder vectors (layer 3)
  domotica train-encoder -data d -cache c -o h     trains the layer-3 head on cached vectors
  domotica templates [-lang L]                     lists the predefined demo commands
  domotica eval-llm -von G -data t.jsonl           layer 4 (VON LLM) vs doing nothing on what
                                                   escalates; its record enables layer 4
`

func manifest() plugin.Manifest {
	return plugin.Manifest{
		ManifestVersion: plugin.ManifestVersion,
		Name:            "domotica",
		Version:         strings.TrimPrefix(Version, "v"),
		// Hasta v0.12 `domotica` era un case del núcleo, que gana a cualquier
		// extensión: con un núcleo anterior esta nunca recibiría el comando.
		MinKling: "0.13.0",
		Summary:  "smart-home demo tools: decide, evaluate and train the domotica layers",
		Commands: []plugin.Command{{
			Name:        "domotica",
			Group:       "SMART-HOME DEMO (docs/domotica.md)",
			Summary:     "decide, evaluate and train the smart-home demo layers",
			Usage:       domoticaHelp,
			Subcommands: domoticaSubcommands,
		}},
	}
}

func main() {
	plugin.Main(manifest(), map[string]func([]string) error{"domotica": cmdDomotica}, nil)
}
