package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

// cmdExport convierte el fichero de confirmaciones en etiquetas limpias, una
// por log (la última confirmación de cada log gana): {"text", "fields",
// "label", "by"}. Es lo que aceptan `kling chispa train -data` y la
// importación de etiquetas humanas de la mejora continua
// (`kling ai feedback <tarea> -import`). Por defecto deja fuera "flaky": el
// modelo de categoría no la puede aprender de un solo log (-flaky la incluye).
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	flaky := fs.Bool("flaky", false, "keep the \"flaky\" confirmations")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: ci-triage export [-flaky] <feedback.jsonl> > labels.jsonl")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	type row struct {
		Text   string         `json:"text"`
		Fields map[string]any `json:"fields,omitempty"`
		Label  string         `json:"label"`
		By     string         `json:"by"`
	}
	var order []string
	last := map[string]row{}
	sc := bufio.NewScanner(io.LimitReader(f, 256<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n, skipped := 0, 0
	for sc.Scan() {
		n++
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var fb triage.Feedback
		if err := json.Unmarshal(sc.Bytes(), &fb); err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
		if fb.Schema != triage.FeedbackSchema || !triage.ValidHumanCategory(fb.Label) || fb.Text == "" ||
			(fb.Label == triage.CatFlaky && !*flaky) {
			skipped++
			continue
		}
		key := fb.LogSHA256
		if key == "" {
			key = fb.Text
		}
		if _, ok := last[key]; !ok {
			order = append(order, key)
		}
		last[key] = row{Text: fb.Text, Fields: fb.Fields, Label: fb.Label, By: "ci-triage"}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(w)
	for _, k := range order {
		if err := enc.Encode(last[k]); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "%d labels from %d confirmations (%d skipped)\n", len(order), n, skipped)
	return w.Flush()
}
