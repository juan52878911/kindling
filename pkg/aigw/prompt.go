package aigw

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/von"
)

// Unknown es la etiqueta cuando la respuesta de VON no es exactamente una de
// las válidas. No se adivina: una etiqueta inventada que pasa por buena es
// peor que un "no sé" que el cliente puede tratar.
const Unknown = "unknown"

const (
	defaultSystem = "You are a strict classifier. Reply with exactly one label from the list and nothing else."
	defaultPrompt = "Labels: {labels}\n\nText:\n{text}\n{fields}\nLabel:"
	// maxPromptText acota el texto que se pega en la pregunta. El contexto de
	// una réplica son 2048 tokens por defecto; 6000 bytes son ~1500 tokens de
	// inglés y dejan sitio a la plantilla y a las etiquetas.
	maxPromptText = 6000
	// maxAnswerShown es lo que se devuelve de la respuesta en crudo de VON.
	maxAnswerShown = 200
)

// renderPrompt rellena la plantilla. Un solo pase de strings.Replacer: lo que
// traiga el texto del usuario ("{labels}", por ejemplo) no se vuelve a
// expandir.
func renderPrompt(tmpl string, labels []string, in chispa.Input, cands []chispa.ClassProb) string {
	if tmpl == "" {
		tmpl = defaultPrompt
	}
	fields := ""
	if len(in.Fields) > 0 {
		if b, err := json.Marshal(in.Fields); err == nil { // Marshal ordena las claves
			fields = "Fields: " + string(b) + "\n"
		}
	}
	var cs []string
	for i, c := range cands {
		if i == 3 {
			break
		}
		cs = append(cs, fmt.Sprintf("%s (%.2f)", c.Label, c.Prob))
	}
	return strings.NewReplacer(
		"{labels}", strings.Join(labels, ", "),
		"{text}", truncUTF8(in.Text, maxPromptText),
		"{fields}", fields,
		"{candidates}", strings.Join(cs, ", "),
	).Replace(tmpl)
}

// renderGenerate rellena la plantilla de una generación: {input} y las
// variables del cliente, en un solo pase (lo que traiga el texto del usuario
// no se vuelve a expandir). Sin plantilla, la pregunta es el input tal cual.
func renderGenerate(tmpl, input string, vars map[string]string) string {
	if tmpl == "" {
		tmpl = "{input}"
	}
	pairs := []string{"{input}", truncUTF8(input, maxText)}
	for _, k := range sortedKeys(vars) {
		pairs = append(pairs, "{"+k+"}", vars[k])
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// parseLabel convierte la respuesta de VON en una de las etiquetas, o Unknown.
//
// Estricto a propósito: primera línea, sin espacios ni la puntuación que un
// modelo pone alrededor (comillas, asteriscos, punto final) y sin un "label:"
// delante, y entonces tiene que coincidir con una etiqueta ENTERA (sin
// distinguir mayúsculas). "fix" vale; "fix." vale; "it's a fix" no. Buscar la
// etiqueta dentro de la frase acertaría a veces y otras cogería la palabra
// equivocada ("not a fix, a feat") sin decirlo. Con la gramática activada la
// respuesta ya es una etiqueta exacta; esto es la defensa por si no lo es,
// porque el invitado no es de fiar.
func parseLabel(answer string, labels []string) string {
	s := strings.TrimSpace(answer)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const punct = " \t\"'`*.:;,!()[]{}<>"
	s = strings.Trim(s, punct)
	if len(s) > 6 && strings.EqualFold(s[:6], "label:") {
		s = strings.Trim(s[6:], punct)
	}
	for _, l := range labels {
		if strings.EqualFold(s, l) {
			return l
		}
	}
	return Unknown
}

// grammarFor es una gramática GBNF de llama-server que solo admite una de las
// etiquetas, exacta. Con ella un modelo de 360M no puede contestar otra cosa.
func grammarFor(labels []string) string {
	alts := make([]string, len(labels))
	for i, l := range labels {
		alts[i] = gbnfString(l)
	}
	return "root ::= " + strings.Join(alts, " | ")
}

// gbnfString escribe s como literal de GBNF: comillas y barras escapadas, y los
// caracteres de control como \xHH para que una etiqueta rara no rompa la
// gramática (las del registro no los admiten; las de un .chispa, en teoría sí).
func gbnfString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// truncUTF8 corta s a como mucho n bytes sin partir una runa.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Prefixes son los prefijos fijos de las tareas que preguntan al modelo VON
// model (por su nombre en el registro): el system prompt de cada una y el texto
// fijo con el que empieza su plantilla, hasta la primera variable. Es lo que
// `kling ai prime` deja evaluado en el dorado del modelo, para que la primera
// petición de cada tarea en una réplica recién restaurada solo evalúe lo que
// cambia (docs/von-cpu.md). Sin repetir y en orden de nombre de tarea.
func (c *Config) Prefixes(model string) []von.Prefix {
	var out []von.Prefix
	seen := map[von.Prefix]bool{}
	for _, n := range sortedKeys(c.Tasks) {
		t := c.Tasks[n]
		var p von.Prefix
		switch {
		case t.VON == model:
			p = von.Prefix{System: t.System, User: staticPrefix(t.Prompt)}
		case t.EscalateTo == model:
			// Una escalada lleva siempre system (el suyo o el de por defecto).
			p = von.Prefix{System: t.System, User: staticPrefix(t.Prompt)}
			if p.System == "" {
				p.System = defaultSystem
			}
			if t.Prompt == "" {
				p.User = staticPrefix(defaultPrompt)
			}
		default:
			continue
		}
		if (p.System == "" && p.User == "") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// staticPrefix es el texto de una plantilla hasta su primera variable: lo que
// todas las peticiones de la tarea comparten en el turno del usuario.
func staticPrefix(tmpl string) string {
	if i := strings.IndexByte(tmpl, '{'); i >= 0 {
		return tmpl[:i]
	}
	return tmpl
}
