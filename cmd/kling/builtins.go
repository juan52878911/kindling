package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// EXTENSIONES INCORPORADAS: `ai`, `chispa` y `models`.
//
// Siguen dentro del binario —pkg/aigw, pkg/chispa y pkg/von son del núcleo y
// no merece la pena un segundo binario para ellos—, pero pasan por el mismo
// camino que una extensión externa: `kling plugins ls` los lista como "built
// in", su ayuda y su completado salen de este manifiesto (una sola fuente, en
// vez de un bloque en main.go y otra lista en completion.go que se desfasaban)
// y quien no los quiere los apaga con `kling plugins disable <nombre>`.

// builtinGroup es la sección de `kling help` donde aparecen los tres.
const builtinGroup = "AI"

const modelsHelp = `  models ls [-json]                                VON catalog and the models on this daemon
  models add <name> -model ID [-quant Q]           builds the image (llama.cpp + GGUF) and a
      [-ctx N] [-cpus N] [-mem MiB] [-replace]     golden snapshot with the model loaded and
      [-url HF_URL -sha256 H] [-rebuild]           warm; serve it with run -from <name>
      [-build-only]                                only the image (to copy it to macOS)
      [-prefix system.txt]... [-cache-ram MiB]     leaves task prompts evaluated in the golden
  models ask <ref> [-max-tokens N] <prompt...>     asks a replica, prints answer and tok/s
  models embed <ref> <text...>                     embedding vector from an encoder replica
  models rm <name> [-keep-image]                   removes its snapshot and image
`

const chispaHelp = `  chispa train -data d.jsonl -o m.chispa [-valid v]  tiny linear classifier: trains,
                                                   quantizes, calibrates, picks τ
  chispa eval -model m.chispa -data t.jsonl [-json]  accuracy, F1, ECE, coverage at τ
  chispa predict -model m.chispa [-text T] [-top N]  label, calibrated p, confident or
                                                   escalate, evidence (docs/chispa.md)
  chispa inspect <m.chispa> [-json]                spec, labels, thresholds, metadata
  chispa deploy <task> -model m.chispa             serverless: a golden snapshot with the
      [-slots s.chispas] [-mem 64] [-vcpus 1]      model loaded (docs/chispa-serverless.md)
  chispa ls [-json] | rm <task> [-keep-image]      deployed tasks / remove one
`

const aiHelp = `  ai up [-config ai.json] [-listen ADDR]           the gateway on http://127.0.0.1:8080 with
                                                   ./ai.json or ~/.config/kling/ai.json; prints
                                                   the URL and a curl to try it
  ai serve [-config ai.json] [-socket S]           serves /v1/classify, /v1/decide,
      [-listen ADDR] [-idle 2m] [-max-replicas 2]  /v1/generate and an OpenAI API; replicas
      [-keepwarm N] [-chispa-mem MiB]              wake per request and freeze when idle
  ai ls [-json]                                    models, tasks, cascades, samples
  ai test <task> [-mode cascade|chispa] <text>     classifies one text through the gateway
  ai generate <task> [-var k=v] [<input>]          runs a generation task (stdin if no input)
  ai eval <task> -data t.jsonl [-von M]            Chispa alone vs the Chispa -> VON cascade on
                                                   labelled data; the record gates escalate_to
  ai calibrate <task> [-target P] [-dry-run]       re-tunes Chispa thresholds on recent VON
                                                   answers; writes only if it improves
  ai reload                                        rereads the registry (says which cascades
                                                   are on, forced or refused)
  ai prime [<model>...] [-dry-run]                 remakes each VON golden snapshot with its
                                                   tasks' prompt prefixes already evaluated
  ai review <task> [-n 20] [-i]                    captured escalations a person should
                                                   confirm or correct (docs/mejora-continua.md)
  ai feedback <task> -id ID -label L               records a human label (or -discard,
      [-teacher NAME] [-import labels.jsonl]       a teacher's answer, or a batch)
  ai retrain <task> [-dry-run] [-rule R]           trains a shadow Chispa on gold + human +
                                                   validated teachers; promotes it only if it
                                                   wins on the trusted held-out set
  ai rollback <task> [-to vN]                      serves the previous (or given) version
`

// builtinExtensions son las extensiones que vienen dentro de kling, en el
// orden en que salen en la ayuda. Cada llamada construye valores nuevos: el
// registro guarda punteros y un test no debe poder ensuciar al siguiente.
func builtinExtensions() []*plugin.Builtin {
	return []*plugin.Builtin{
		newBuiltin("ai", "AI gateway: Chispa classifies, VON generates, models wake on demand (docs/ai-gateway.md)",
			aiHelp, []string{"up", "serve", "ls", "test", "generate", "eval", "calibrate", "reload",
				"prime", "review", "feedback", "retrain", "rollback"}, cmdAI),
		newBuiltin("chispa", "Chispa: tiny local classifiers, no daemon needed (docs/chispa.md)",
			chispaHelp, []string{"train", "eval", "predict", "inspect", "deploy", "ls", "rm"}, cmdChispa),
		newBuiltin("models", "VON: small LLMs with an OpenAI-compatible API on port 8000",
			modelsHelp, []string{"ls", "add", "ask", "embed", "rm"}, cmdModels),
	}
}

// newBuiltin arma una incorporada con un solo comando que se llama como ella.
// Envuelve run para que `kling help <cmd>` —que llega como `<cmd> -h`— imprima
// la ayuda del manifiesto: así la ayuda general y la del comando no pueden
// contar cosas distintas.
func newBuiltin(name, summary, usage string, subs []string, run func([]string) error) *plugin.Builtin {
	return &plugin.Builtin{
		Manifest: plugin.Manifest{
			ManifestVersion: plugin.ManifestVersion,
			Name:            name,
			Version:         Version,
			Summary:         summary,
			Commands: []plugin.Command{{
				Name:        name,
				Group:       builtinGroup,
				Summary:     summary,
				Usage:       usage,
				Subcommands: subs,
			}},
		},
		Commands: map[string]func([]string) error{
			name: func(args []string) error {
				if len(args) == 1 && isHelpArg(args[0]) {
					writeBuiltinHelp(os.Stdout, name, summary, usage)
					return nil
				}
				return run(args)
			},
		},
	}
}

func isHelpArg(a string) bool { return a == "-h" || a == "--help" || a == "help" }

func writeBuiltinHelp(w io.Writer, name, summary, usage string) {
	fmt.Fprintf(w, "kling %s - %s\n\nUSAGE\n%s", name, summary, usage)
	if !strings.HasSuffix(usage, "\n") {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\nRun 'kling %s <subcommand> -h' for the options of each one.\n", name)
}
