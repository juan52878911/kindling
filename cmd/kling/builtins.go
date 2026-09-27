package main

import (
	"strings"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// EXTENSIÓN INCORPORADA: `ai`.
//
// El gateway de IA (pkg/aigw), los clasificadores Chispa (pkg/chispa) y los
// LLM pequeños de VON (pkg/von) siguen dentro del binario —no merece la pena
// un segundo ejecutable para ellos—, pero pasan por el mismo camino que una
// extensión externa: `kling plugin ls` la lista como "built in", su ayuda y
// su completado salen de este manifiesto (una sola fuente) y quien no la
// quiere la apaga con `kling plugin disable ai`.
//
// Hasta 0.13 eran tres extensiones al mismo nivel (ai, chispa, models) y el
// usuario tenía que saber que "modelo" se decía `models add` para un LLM y
// `chispa deploy` para un clasificador. Ahora es un solo espacio: `ai model`
// y `ai chispa`; `models` y `chispa` a secas son alias silenciosos (tree.go).

// aiCommands es lo que `kling ai` ofrece, en el orden de la ayuda.
var aiCommands = []plugin.Command{
	{Name: "up", Group: "GATEWAY", Summary: "starts the gateway with sensible defaults and prints its URL", Usage: `  ai up [-config ai.json] [-listen ADDR]           the gateway on http://127.0.0.1:8080
                                                   with ./ai.json or ~/.config/kling/
                                                   ai.json; prints the URL and a curl
`},
	{Name: "serve", Group: "GATEWAY", Summary: "serves classify, decide, generate and an OpenAI API", Usage: `  ai serve [-config ai.json] [-socket S]           serves /v1/classify, /v1/decide,
      [-listen ADDR] [-idle 2m] [-max-replicas 2]  /v1/generate and an OpenAI API;
      [-keepwarm N] [-chispa-mem 64M]              replicas wake per request and
                                                   freeze when idle
`},
	{Name: "ls", Group: "GATEWAY", Summary: "models, tasks, cascades, samples", Usage: `  ai ls [-json]                                    models, tasks, cascades, samples
`},
	{Name: "test", Group: "GATEWAY", Summary: "classifies one text through the gateway", Usage: `  ai test <task> [-mode cascade|chispa] <text>     classifies one text through the
                                                   gateway
`},
	{Name: "generate", Group: "GATEWAY", Summary: "runs a generation task", Usage: `  ai generate <task> [-var k=v] [<input>]          runs a generation task (stdin if
                                                   no input)
`},
	{Name: "eval", Group: "GATEWAY", Summary: "Chispa alone vs the Chispa -> VON cascade on labelled data", Usage: `  ai eval <task> -data t.jsonl [-von M]            Chispa alone vs the Chispa -> VON
                                                   cascade on labelled data; the
                                                   record gates escalate_to
`},
	{Name: "calibrate", Group: "GATEWAY", Summary: "re-tunes Chispa thresholds on recent VON answers", Usage: `  ai calibrate <task> [-target P] [-dry-run]       re-tunes Chispa thresholds on
                                                   recent VON answers; writes only if
                                                   it improves
`},
	{Name: "reload", Group: "GATEWAY", Summary: "rereads the registry", Usage: `  ai reload                                        rereads the registry (says which
                                                   cascades are on, forced or refused)
`},
	{Name: "prime", Group: "GATEWAY", Summary: "remakes each VON template with its prompt prefixes evaluated", Usage: `  ai prime [<model>...] [-dry-run]                 remakes each VON template with its
                                                   tasks' prompt prefixes already
                                                   evaluated
`},
	{Name: "review", Group: "CONTINUOUS IMPROVEMENT", Summary: "captured escalations a person should confirm or correct", Usage: `  ai review <task> [-n 20] [-i]                    captured escalations a person
                                                   should confirm or correct
                                                   (docs/mejora-continua.md)
`},
	{Name: "feedback", Group: "CONTINUOUS IMPROVEMENT", Summary: "records a human label", Usage: `  ai feedback <task> -id ID -label L               records a human label (or
      [-teacher NAME] [-import labels.jsonl]       -discard, a teacher's answer, or
                                                   a batch)
`},
	{Name: "retrain", Group: "CONTINUOUS IMPROVEMENT", Summary: "trains a shadow Chispa and promotes it only if it wins", Usage: `  ai retrain <task> [-dry-run] [-rule R]           trains a shadow Chispa on gold +
                                                   human + validated teachers;
                                                   promotes it only if it wins on the
                                                   trusted held-out set
`},
	{Name: "rollback", Group: "CONTINUOUS IMPROVEMENT", Summary: "serves the previous (or given) version", Usage: `  ai rollback <task> [-to vN]                      serves the previous (or given)
                                                   version
`},
	{Name: "model", Group: "MODELS (VON: small LLMs with an OpenAI-compatible API on port 8000)", Summary: "small LLMs as templates: add, ask, embed",
		Subcommands: []string{"ls", "add", "ask", "embed", "rm"}, MachineArgs: []string{"ask", "embed"}, Usage: `  ai model ls [-q] [-json]                         VON catalog and the models on this
                                                   daemon
  ai model add <name> -model ID [-quant Q]         builds the image (llama.cpp + GGUF)
      [-ctx N] [-cpus N] [-mem 2G] [-replace]      and a template with the model
      [-url HF_URL -sha256 H] [-rebuild]           loaded and warm; serve it with
      [-build-only]                                run -from <name>; -build-only:
      [-prefix system.txt]... [-cache-ram 512M]    only the image (to copy it to
                                                   macOS); -prefix: task prompts
                                                   evaluated in the template
  ai model ask <ref> [-max-tokens N] <prompt...>   asks a replica, prints answer and
                                                   tok/s
  ai model embed <ref> <text...>                   embedding vector from an encoder
                                                   replica
  ai model rm [-f] <name> [-keep-image]            removes its template and image
`},
	{Name: "chispa", Group: "CLASSIFIERS (Chispa: tiny local classifiers, no daemon needed; docs/chispa.md)", Summary: "tiny linear classifiers: train, eval, predict, deploy",
		Subcommands: []string{"train", "eval", "predict", "inspect", "deploy", "ls", "rm"}, Usage: `  ai chispa train -data d.jsonl -o m.chispa        trains, quantizes, calibrates,
      [-valid v]                                   picks τ
  ai chispa eval -model m.chispa -data t.jsonl     accuracy, F1, ECE, coverage at τ
      [-json]
  ai chispa predict -model m.chispa [-text T]      label, calibrated p, confident or
      [-top N]                                     escalate, evidence
  ai chispa inspect <m.chispa> [-json]             spec, labels, thresholds, metadata
  ai chispa deploy <task> -model m.chispa          serverless: a template with the
      [-slots s.chispas] [-mem 64M] [-cpus 1]      model loaded
                                                   (docs/chispa-serverless.md)
  ai chispa ls [-q] [-json]                        deployed tasks
  ai chispa rm [-f] <task> [-keep-image]           removes one
`},
}

// builtinExtensions son las extensiones que vienen dentro de kling. Cada
// llamada construye valores nuevos: el registro guarda punteros y un test no
// debe poder ensuciar al siguiente.
func builtinExtensions() []*plugin.Builtin {
	cmds := map[string]func([]string) error{}
	for _, c := range aiCommands {
		name := c.Name
		cmds[name] = func(args []string) error { return cmdAI(append([]string{name}, args...)) }
	}
	return []*plugin.Builtin{{
		Manifest: plugin.Manifest{
			ManifestVersion: plugin.ManifestVersion,
			Name:            "ai",
			Version:         strings.TrimPrefix(Version, "v"),
			Summary:         "AI gateway: Chispa classifies, VON generates, models wake on demand (docs/ai-gateway.md)",
			HelpGroup:       "SERVE",
			Commands:        aiCommands,
		},
		Commands: cmds,
	}}
}
