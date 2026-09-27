package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// finishRow deja una fila lista: fuera de ámbito no lleva huecos; dentro, se
// marcan además los nombres de dispositivo que la fuente dejó sin anotar (en
// MASSIVE y en las plantillas de HA «las luces» es texto literal, no un hueco:
// sin esto el etiquetador vería la misma palabra como hueco en unas frases y
// como nada en otras) y los valores normalizados salen de los huecos.
func finishRow(r *domotica.Row, defaultDevice string) {
	if r.Intent == domotica.OutOfScope {
		r.Spans, r.Slots = nil, domotica.Slots{}
		return
	}
	for _, d := range domotica.FindSpans(r.Text, domotica.SlotDevice) {
		overlap := false
		for _, s := range r.Spans {
			if d.Start < s.End && d.End > s.Start {
				overlap = true
				break
			}
		}
		if !overlap {
			r.Spans = append(r.Spans, slots.Span{Slot: d.Slot, Start: d.Start, End: d.End})
		}
	}
	sort.Slice(r.Spans, func(i, j int) bool { return r.Spans[i].Start < r.Spans[j].Start })
	s := domotica.SlotsFromSpans(r.Text, r.Spans)
	if s.Device == "" {
		s.Device = defaultDevice
	}
	r.Slots = domotica.Resolve(r.Intent, s)
}

func seedOf(family string, i int) uint64 {
	return chispa.FNV1a64(fmt.Sprintf("%s#%d", family, i)) | 1
}

// splitPattern reparte las familias de un grupo (misma fuente e intención),
// ordenadas por hash, en 7 train / 1 valid / 2 test de cada 10. Estratificar
// así garantiza que una intención con al menos dos familias tenga alguna en
// test; repartir cada familia por su hash sin más dejaba intenciones enteras
// fuera de test. Determinista y sin que una familia cruce de reparto.
var splitPattern = [10]string{"train", "test", "train", "valid", "train", "train", "test", "train", "train", "train"}

func assignSplits(rows []domotica.Row) {
	type fam struct {
		name string
		hash uint64
	}
	groups := map[string][]fam{}
	seenFam := map[string]bool{}
	for _, r := range rows {
		if seenFam[r.Family] {
			continue
		}
		seenFam[r.Family] = true
		g := r.Source + "/" + r.Intent
		groups[g] = append(groups[g], fam{r.Family, chispa.FNV1a64("split:" + r.Family)})
	}
	split := map[string]string{}
	for _, fs := range groups {
		sort.Slice(fs, func(i, j int) bool {
			return fs[i].hash < fs[j].hash || fs[i].hash == fs[j].hash && fs[i].name < fs[j].name
		})
		for i, f := range fs {
			split[f.name] = splitPattern[i%len(splitPattern)]
		}
	}
	for i := range rows {
		rows[i].Split = split[rows[i].Family]
	}
}

// Listas con las que se generan las frases de la demo.
var demoAreas = haAreas

var demoColors = map[string][]string{
	"es": {"blanco", "rojo", "azul", "verde", "amarillo", "naranja", "morado", "rosa", "lila", "violeta", "celeste"},
	"en": {"white", "red", "blue", "green", "yellow", "orange", "purple", "pink", "violet", "cyan"},
}

var perDemoTemplate = 20

func loadDemo() ([]domotica.Row, error) {
	var out []domotica.Row
	grammars := map[string]*domotica.Grammar{}
	for i, t := range domotica.DemoTemplates {
		g := grammars[t.Lang]
		if g == nil {
			g = &domotica.Grammar{Lang: t.Lang, Rules: domotica.DemoRules(t.Lang), Lists: map[string]*domotica.List{}}
			var areas, colors []domotica.ListValue
			for _, a := range demoAreas[t.Lang] {
				areas = append(areas, domotica.ListValue{In: a, Out: a})
			}
			for _, c := range demoColors[t.Lang] {
				colors = append(colors, domotica.ListValue{In: c, Out: c})
			}
			g.Lists["area"] = &domotica.List{Values: areas}
			g.Lists["color"] = &domotica.List{Values: colors}
			grammars[t.Lang] = g
		}
		g.Lists["value"] = &domotica.List{Range: true, From: 0, To: 100}
		if strings.Contains(t.Intent, "temperature") {
			g.Lists["value"] = &domotica.List{Range: true, From: 15, To: 30, Halves: true}
		}
		n, err := domotica.Parse(t.Template)
		if err != nil {
			return nil, err
		}
		family := fmt.Sprintf("demo/%d", i)
		rng := &domotica.Rand{S: seedOf(family, 0)}
		seen := map[string]bool{}
		for try := 0; try < perDemoTemplate*6 && len(seen) < perDemoTemplate; try++ {
			e, err := g.Sample(n, rng)
			if err != nil {
				return nil, fmt.Errorf("demo %q: %w", t.Template, err)
			}
			norm := domotica.Norm(e.Text)
			if seen[norm] {
				continue
			}
			seen[norm] = true
			r := domotica.Row{Text: e.Text, Lang: t.Lang, Intent: t.Intent, Source: "demo", Family: family}
			for _, c := range e.Captures {
				r.Spans = append(r.Spans, slots.Span{Slot: c.Slot, Start: c.Start, End: c.End})
			}
			finishRow(&r, t.Device)
			out = append(out, r)
		}
	}
	return out, nil
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	cache := fs.String("cache", defaultCache(), "cache directory (from fetch)")
	outDir := fs.String("out", "", "output directory (default CACHE/data)")
	fs.IntVar(&perTemplateInScope, "ha-k", perTemplateInScope, "sentences per in-scope home-assistant template")
	fs.IntVar(&perTemplateOOS, "ha-k-oos", perTemplateOOS, "sentences per out-of-scope home-assistant template")
	fs.IntVar(&perDemoTemplate, "demo-k", perDemoTemplate, "sentences per demo template")
	_ = fs.Parse(args)
	if *outDir == "" {
		*outDir = filepath.Join(*cache, "data")
	}

	mfiles, err := readTar(filepath.Join(*cache, sources[0].File), sources[0], func(n string) bool {
		return n == "1.0/data/es-ES.jsonl" || n == "1.0/data/en-US.jsonl" || n == "1.0/LICENSE" ||
			n == "1.0/NOTICE.md" || n == "1.0/CITATION.md"
	})
	if err != nil {
		return err
	}
	massive, err := loadMassive(mfiles)
	if err != nil {
		return err
	}
	hraw, err := readTar(filepath.Join(*cache, sources[1].File), sources[1], func(n string) bool {
		n = stripTop(n)
		return n == "LICENSE.md" || strings.HasPrefix(n, "sentences/es/") || strings.HasPrefix(n, "sentences/en/") ||
			strings.HasPrefix(n, "rules/es/") || strings.HasPrefix(n, "rules/en/") || strings.HasPrefix(n, "lists/")
	})
	if err != nil {
		return err
	}
	hfiles := map[string][]byte{}
	for k, v := range hraw {
		hfiles[stripTop(k)] = v
	}
	var hst haStats
	ha, err := loadHA(hfiles, &hst)
	if err != nil {
		return err
	}
	demo, err := loadDemo()
	if err != nil {
		return err
	}

	// Repartos sin fugas: MASSIVE conserva los oficiales; HA y la demo, por
	// familia. Una frase de HA o de la demo que ya existe (normalizada, en el
	// mismo idioma) en otro sitio se descarta: si no, la misma frase podría
	// estar en train y en test.
	seen := map[string]string{}
	var rows []domotica.Row
	dups := map[string]int{}
	for _, r := range massive {
		seen[r.Lang+"\x00"+domotica.Norm(r.Text)] = r.Split
		rows = append(rows, r)
	}
	assignSplits(ha)
	assignSplits(demo)
	for _, group := range [][]domotica.Row{ha, demo} {
		for _, r := range group {
			key := r.Lang + "\x00" + domotica.Norm(r.Text)
			if _, dup := seen[key]; dup {
				dups[r.Source]++
				continue
			}
			seen[key] = r.Split
			rows = append(rows, r)
		}
	}

	if err := os.MkdirAll(filepath.Join(*outDir, "chispa"), 0o755); err != nil {
		return err
	}
	counts := map[string]map[string]int{} // split → source/lang → n
	intents := map[string]map[string]int{}
	for _, split := range []string{"train", "valid", "test"} {
		uf, err := os.Create(filepath.Join(*outDir, split+".jsonl"))
		if err != nil {
			return err
		}
		jf, err := os.Create(filepath.Join(*outDir, "chispa", split+".jsonl"))
		if err != nil {
			return err
		}
		uw, jw := bufio.NewWriter(uf), bufio.NewWriter(jf)
		ue, je := json.NewEncoder(uw), json.NewEncoder(jw)
		ue.SetEscapeHTML(false)
		je.SetEscapeHTML(false)
		counts[split] = map[string]int{}
		intents[split] = map[string]int{}
		for _, r := range rows {
			if r.Split != split {
				continue
			}
			if err := ue.Encode(r); err != nil {
				return err
			}
			if err := je.Encode(chispa.Example{Text: r.Text, Label: r.Intent, Fields: map[string]any{"lang": r.Lang}}); err != nil {
				return err
			}
			counts[split][r.Source+"/"+r.Lang]++
			intents[split][r.Intent]++
		}
		for _, f := range []func() error{uw.Flush, jw.Flush, uf.Close, jf.Close} {
			if err := f(); err != nil {
				return err
			}
		}
	}
	// Las licencias viajan con los datos derivados.
	lic := filepath.Join(*outDir, "LICENSES")
	if err := os.MkdirAll(lic, 0o755); err != nil {
		return err
	}
	for src, dst := range map[string]string{"1.0/LICENSE": "MASSIVE-LICENSE", "1.0/NOTICE.md": "MASSIVE-NOTICE.md", "1.0/CITATION.md": "MASSIVE-CITATION.md"} {
		if err := os.WriteFile(filepath.Join(lic, dst), mfiles[src], 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(lic, "HA-INTENTS-LICENSE.md"), hfiles["LICENSE.md"], 0o644); err != nil {
		return err
	}

	fmt.Printf("home-assistant/intents: %d files (%d unreadable), %d blocks, %d templates, %d skipped, %d rows, %d expansions dropped\n",
		hst.files, hst.skippedFiles, hst.blocks, hst.templates, hst.skippedTemplates, hst.rows, hst.dropped)
	for _, k := range sortedKeys(hst.skipReasons) {
		fmt.Printf("  skipped: %-40s %d\n", k, hst.skipReasons[k])
	}
	fmt.Printf("duplicates dropped (already in another source or split): %v\n", dups)
	for _, split := range []string{"train", "valid", "test"} {
		total := 0
		for _, n := range counts[split] {
			total += n
		}
		fmt.Printf("%-5s %6d rows  %v\n", split, total, counts[split])
	}
	fmt.Println("intents (train / valid / test):")
	for _, it := range domotica.IntentNames() {
		fmt.Printf("  %-20s %6d %6d %6d\n", it, intents["train"][it], intents["valid"][it], intents["test"][it])
	}
	fmt.Printf("written to %s\n", *outDir)
	return nil
}
