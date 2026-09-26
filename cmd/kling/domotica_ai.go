package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/domotica"
)

// `kling domotica` se fue a la extensión kling-domotica
// (examples/domotica/cmd/kling-domotica), pero `kling ai eval` de una tarea de
// domótica sigue en el núcleo: es la evaluación que enciende la capa 3 de una
// tarea del gateway, y el gateway (pkg/aigw) es del núcleo. Por eso esto se
// queda aquí y no en la extensión.

// readRowsFile lee el JSONL unificado de domótica y devuelve también su
// sha256, que queda en el registro de la evaluación.
func readRowsFile(path string) ([]domotica.Row, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	h := sha256.New()
	rows, err := domotica.ReadRows(io.TeeReader(f, h))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return rows, hex.EncodeToString(h.Sum(nil)), nil
}

// isDomoticaTask pregunta al gateway si la tarea es de domótica (sin
// gateway, o sin la tarea, no lo es y el camino de siempre da el error).
func isDomoticaTask(c *aiClient, task string) bool {
	var live struct {
		Tasks []aigw.TaskInfo `json:"tasks"`
	}
	if c.do(http.MethodGet, "/v1/tasks", nil, &live) != nil {
		return false
	}
	for _, t := range live.Tasks {
		if t.Name == task {
			return t.Kind == "domotica"
		}
	}
	return false
}

// aiEvalDomotica es `kling ai eval` de una tarea de domótica: filas del JSONL
// unificado (texto, idioma, intención, huecos) por la cascada sin y con la
// capa 3; el registro enciende (o no) el codificador.
func aiEvalDomotica(c *aiClient, task, data string, dry, asJSON bool) error {
	rows, _, err := readRowsFile(data)
	if err != nil {
		return err
	}
	c.http.Timeout = 0
	fmt.Fprintf(os.Stderr, "evaluating %d rows on task %s (fast layers vs with the encoder)...\n", len(rows), task)
	var out struct {
		Record  aigw.DomoticaEvalRecord `json:"record"`
		Cascade aigw.CascadeState       `json:"cascade"`
	}
	if err := c.do(http.MethodPost, "/v1/admin/eval", aigw.EvalRequest{
		Task: task, Data: filepath.Base(data), Rows: rows, DryRun: dry}, &out); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	r, res := out.Record, out.Record.Results
	fmt.Printf("task %s: intent %s, encoder %s (%s), %d rows\n", r.Task, r.Intent.Model, r.Encoder.Model, r.Encoder.Snapshot, res.Examples)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "\tanswered right\tconfident\tright when confident\tconfident errors\texact (with guesses)")
	fmt.Fprintf(tw, "fast layers\t%.3f\t%.3f\t%.3f\t%d\t%.3f\n", res.FastAnswered, res.FastCoverage, res.FastPrecision, res.FastConfidentWrong, res.FastExact)
	fmt.Fprintf(tw, "with the encoder\t%.3f\t%.3f\t%.3f\t%d\t%.3f\n", res.CascadeAnswered, res.CascadeCoverage, res.CascadePrecision, res.CascadeConfidentWrong, res.CascadeExact)
	_ = tw.Flush()
	fmt.Printf("to the encoder %d (confident %d, request errors %d)\n", res.ToEncoder, res.EncoderConfident, res.EncoderErrors)
	fmt.Printf("answered right: fast only %d, with the encoder only %d (McNemar p=%.2g); counting escalated guesses: %d vs %d (p=%.2g)\n",
		res.FastOnlyRight, res.CascadeOnlyRight, res.PValue, res.ExactFastOnlyRight, res.ExactCascadeOnlyRight, res.ExactPValue)
	fmt.Printf("encoder latency p50 %.1f ms, p95 %.1f ms (%.0f s in total)\n", res.EncoderP50MS, res.EncoderP95MS, res.DurationS)
	fmt.Println(r.Verdict)
	if r.Stored != "" {
		fmt.Printf("stored in %s; encoder layer %s\n", r.Stored, out.Cascade.Status)
	} else {
		fmt.Println("dry run: nothing stored")
	}
	return nil
}
