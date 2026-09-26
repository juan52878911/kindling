package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
)

// MEJORA CONTINUA: `kling ai review|feedback|retrain|rollback`. Todo pasa por
// el gateway en marcha (es quien tiene las capturas, el modelo que sirve y la
// puerta); el CLI solo pregunta y enseña. Diseño y cifras en
// docs/mejora-continua.md.

func defaultBy() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "cli"
}

func aiReview(args []string) error {
	fs := flag.NewFlagSet("ai review", flag.ExitOnError)
	n := fs.Int("n", 20, "cases to show (at most 500)")
	interactive := fs.Bool("i", false, "go through the cases one by one and record the answers")
	by := fs.String("by", defaultBy(), "who is reviewing (stored with each label)")
	asJSON := fs.Bool("json", false, "JSON output")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling ai review <task> [-n 20] [-i] [-json]")
	}
	task := fs.Arg(0)
	c, err := mk()
	if err != nil {
		return err
	}
	var rv aigw.ReviewResponse
	if err := c.do(http.MethodPost, "/v1/admin/review", aigw.ReviewRequest{Task: task, N: *n}, &rv); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rv)
	}
	fmt.Printf("task %s: %d cases need a person, %d would enter on their own, %d already reviewed\n", task, rv.Pending, rv.Acceptable, rv.Reviewed)
	for _, r := range sortedNames(rv.Reasons) {
		fmt.Printf("  %5d  %s\n", rv.Reasons[r], r)
	}
	printTeachers(rv.Teachers)
	if len(rv.Cases) == 0 {
		fmt.Println("nothing to review")
		return nil
	}
	if !*interactive {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "\nID\tCHISPA\tTEACHERS\tPROPOSED\tWHY\tTEXT")
		for _, k := range rv.Cases {
			why := k.Reason
			if k.RareClass {
				why += " (rare class)"
			}
			fmt.Fprintf(tw, "%s\t%s %.2f\t%s\t%s\t%s\t%s\n", k.ID, k.Chispa.Label, k.Chispa.Prob, votesText(k.Teachers), k.Proposed, why, oneLine(k.Text, 60))
		}
		_ = tw.Flush()
		fmt.Printf("\nconfirm or correct with: kling ai review %s -i   (or kling ai feedback %s -id <id> -label <label>)\n", task, task)
		return nil
	}
	in := bufio.NewReader(os.Stdin)
	done := 0
	for i, k := range rv.Cases {
		fmt.Printf("\n[%d/%d] %s  %s\n", i+1, len(rv.Cases), k.ID, k.Reason)
		fmt.Printf("  text:     %s\n", oneLine(k.Text, 400))
		if len(k.Fields) > 0 {
			b, _ := json.Marshal(k.Fields)
			fmt.Printf("  fields:   %s\n", b)
		}
		var top []string
		for _, cp := range k.Chispa.Top {
			top = append(top, fmt.Sprintf("%s %.2f", cp.Label, cp.Prob))
		}
		fmt.Printf("  chispa:   %s\n", strings.Join(top, ", "))
		fmt.Printf("  teachers: %s\n", votesText(k.Teachers))
		fmt.Printf("label [enter = %s, d = discard, s = skip, q = quit]: ", k.Proposed)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			break
		}
		ans := strings.TrimSpace(line)
		item := aigw.FeedbackItem{ID: k.ID, By: *by}
		switch ans {
		case "q":
			fmt.Printf("%d recorded\n", done)
			return nil
		case "s":
			continue
		case "d":
			item.Action = "discard"
		case "":
			item.Action, item.Label = "confirm", k.Proposed
		default:
			item.Action, item.Label = "correct", ans
		}
		if err := c.do(http.MethodPost, "/v1/feedback", aigw.FeedbackRequest{Task: task, FeedbackItem: item}, nil); err != nil {
			return err
		}
		done++
	}
	fmt.Printf("\n%d recorded\n", done)
	return nil
}

func votesText(vs []aigw.TeacherVote) string {
	if len(vs) == 0 {
		return "-"
	}
	var s []string
	for _, v := range vs {
		s = append(s, v.Name+"="+v.Label)
	}
	return strings.Join(s, " ")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func printTeachers(ts []aigw.TeacherTrust) {
	if len(ts) == 0 {
		fmt.Println("no teacher answers yet: only human labels can teach this task")
		return
	}
	fmt.Println("teachers:")
	for _, t := range ts {
		st := "not validated"
		if t.Validated {
			st = "validated (" + t.Source + ")"
		}
		fmt.Printf("  %-20s %s: %s\n", t.Name, st, t.Reason)
	}
}

func aiFeedback(args []string) error {
	fs := flag.NewFlagSet("ai feedback", flag.ExitOnError)
	id := fs.String("id", "", "the id of a gateway answer")
	label := fs.String("label", "", "the right label")
	discard := fs.Bool("discard", false, "the case should never train (ambiguous, garbage)")
	teacher := fs.String("teacher", "", "report a teacher's answer (stored as ext:<name>, never taken as truth)")
	conf := fs.Float64("conf", 0, "the teacher's confidence, if it has one")
	text := fs.String("text", "", "the text, when there is no id (or the case was captured as a hash)")
	fields := fs.String("fields", "", "structured fields as JSON, with -text")
	by := fs.String("by", defaultBy(), "who labels (stored with the label)")
	imp := fs.String("import", "", `JSONL of labels to import: {"text", "fields"?, "label", "by"?} or {"id", "label"} per line`)
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling ai feedback <task> -id ID -label L | -id ID -discard | -text T -label L | -import labels.jsonl")
	}
	task := fs.Arg(0)
	c, err := mk()
	if err != nil {
		return err
	}
	if *imp != "" {
		return importFeedback(c, task, *imp, *by)
	}
	item := aigw.FeedbackItem{ID: *id, Label: *label, Teacher: *teacher, Conf: *conf, Text: *text, By: *by}
	if *discard {
		item.Action = "discard"
	}
	if *fields != "" {
		if err := json.Unmarshal([]byte(*fields), &item.Fields); err != nil {
			return fmt.Errorf("-fields: %w", err)
		}
	}
	var out aigw.FeedbackResponse
	if err := c.do(http.MethodPost, "/v1/feedback", aigw.FeedbackRequest{Task: task, FeedbackItem: item}, &out); err != nil {
		return err
	}
	fmt.Printf("recorded %d label(s) for task %s\n", out.Recorded, task)
	return nil
}

// importFeedback manda un JSONL de etiquetas en tandas: lo que exporta otra
// herramienta (p. ej. el triaje de CI con etiquetas confirmadas por personas).
func importFeedback(c *aiClient, task, path, by string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 256<<20))
	var batch []aigw.FeedbackItem
	total, size := 0, 0
	send := func() error {
		if len(batch) == 0 {
			return nil
		}
		var out aigw.FeedbackResponse
		if err := c.do(http.MethodPost, "/v1/feedback", aigw.FeedbackRequest{Task: task, Items: batch}, &out); err != nil {
			return fmt.Errorf("after %d imported: %w", total, err)
		}
		total += out.Recorded
		batch, size = batch[:0], 0
		return nil
	}
	for n := 1; ; n++ {
		var it aigw.FeedbackItem
		if err := dec.Decode(&it); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("%s: line %d: %w", path, n, err)
		}
		if it.By == "" && it.Teacher == "" {
			it.By = by
		}
		// Tandas de hasta 500 etiquetas o ~512 KiB: el cuerpo de una petición
		// al gateway está acotado a 1 MiB.
		b, _ := json.Marshal(it)
		if size+len(b) > 512<<10 {
			if err := send(); err != nil {
				return err
			}
		}
		batch = append(batch, it)
		size += len(b)
		if len(batch) == 500 {
			if err := send(); err != nil {
				return err
			}
		}
	}
	if err := send(); err != nil {
		return err
	}
	fmt.Printf("imported %d label(s) into task %s\n", total, task)
	return nil
}

func aiRetrain(args []string) error {
	fs := flag.NewFlagSet("ai retrain", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "train and measure, but never promote")
	var trust stringsFlag
	fs.Var(&trust, "trust-teacher", "use this teacher's labels without validation (repeatable; recorded in the report)")
	rule := fs.String("rule", "mcnemar", "promotion rule: mcnemar (significant win) or no-regression")
	tol := fs.Float64("precision-tolerance", 0.005, "how much confident precision on the held-out set may drop")
	current := fs.String("current", "", "microvm task without versions yet: the .chispa it serves now")
	evalData := fs.String("eval", "", "data to re-evaluate the cascade after promoting (default: learn.heldout; domotica: rows JSONL)")
	noEval := fs.Bool("no-eval", false, "don't re-evaluate the cascade: it stays off until kling ai eval")
	host := hostFlag(fs)
	mem := fs.Int("mem", 64, "microvm task: memory of the new version's golden snapshot (MiB)")
	vcpus := fs.Int("vcpus", 1, "microvm task: vCPUs of the new version's golden snapshot")
	asJSON := fs.Bool("json", false, "JSON output")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling ai retrain <task> [-dry-run] [-trust-teacher NAME] [-rule mcnemar|no-regression] [-eval data.jsonl]")
	}
	task := fs.Arg(0)
	c, err := mk()
	if err != nil {
		return err
	}
	c.http.Timeout = 0 // entrenar y, si hace falta, re-evaluar la cascada: minutos
	req := aigw.RetrainRequest{Task: task, DryRun: *dry, Trust: trust, Rule: *rule, PrecisionTolerance: tol, NoEval: *noEval}
	if *current != "" {
		if req.CurrentModel, err = os.ReadFile(*current); err != nil {
			return err
		}
	}
	if *evalData != "" {
		if isDomoticaTask(c, task) {
			if req.EvalRows, _, err = readRowsFile(*evalData); err != nil {
				return err
			}
		} else if req.EvalExamples, err = readEvalData(*evalData); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "retraining task %s...\n", task)
	var rep aigw.RetrainReport
	if err := c.do(http.MethodPost, "/v1/admin/retrain", req, &rep); err != nil {
		return err
	}
	if *asJSON && !rep.Pending {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}
	if !*asJSON {
		printRetrain(&rep)
	}
	if !rep.Pending {
		return nil
	}
	// Tarea microvm: la versión nueva es un dorado nuevo. Se hace aquí (el
	// gateway no construye imágenes) y se confirma: el gateway comprueba que
	// el dorado sirve exactamente el modelo que pasó la puerta.
	ctx, stop := ctxWithSignals()
	defer stop()
	dc := api.NewClient(hostOf(*host))
	if err := chispaDeploy(ctx, dc, chispaDeployOptions{
		Name: rep.Snapshot, Model: rep.CandidateModel, ModelName: task + "@" + rep.Version,
		MemMiB: *mem, VCPUs: *vcpus, Replace: true, Rebuild: true, Wait: 2 * time.Minute,
	}); err != nil {
		return fmt.Errorf("deploying %s: %w (the version stays pending; retrain again to retry)", rep.Snapshot, err)
	}
	var vc aigw.VersionChange
	if err := c.do(http.MethodPost, "/v1/admin/promote", aigw.PromoteRequest{Task: task, Version: rep.Version, Snapshot: rep.Snapshot}, &vc); err != nil {
		return err
	}
	if *asJSON {
		rep.CandidateModel = nil
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"retrain": rep, "promote": vc})
	}
	fmt.Printf("promoted %s: task %s now serves %s (previous golden kept for kling ai rollback)\n", vc.To, task, vc.Snapshot)
	for _, n := range vc.Cascades {
		fmt.Println("  " + n)
	}
	return nil
}

func printRetrain(rep *aigw.RetrainReport) {
	d := rep.Data
	fmt.Printf("task %s (chispa %s): gold %d, human %d, accepted from teachers %d; pending review %d, discarded %d, over the class cap %d, held-out overlap removed %d\n",
		rep.Task, rep.Model, d.Gold, d.Human, d.Accepted, d.Pending, d.Discarded, d.Capped, d.Leaked)
	printTeachers(rep.Teachers)
	if rep.Shadow.Examples > 0 {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintf(tw, "held-out (%d examples)\tcoverage\tconfident precision\tanswered right\taccuracy\tconfident errors\n", rep.Current.Examples)
		row := func(name string, h aigw.HeldoutMetrics) {
			fmt.Fprintf(tw, "%s\t%.4f\t%.4f\t%.4f\t%.4f\t%d\n", name, h.Coverage, h.ConfidentPrecision, h.AnsweredRight, h.Accuracy, h.ConfidentErrors)
		}
		row("current "+orDash(rep.Current.Version), rep.Current)
		row("new", rep.Shadow)
		_ = tw.Flush()
		fmt.Printf("trained on %d examples (%d for validation) in %.1f s\n", d.Train, d.Valid, rep.TrainSeconds)
	}
	fmt.Printf("gate (%s): %s\n", rep.Gate.Rule, rep.Gate.Verdict)
	switch {
	case rep.Promoted:
		fmt.Printf("promoted %s: wrote %s (previous model in %s)\n", rep.Version, rep.Written, rep.Backup)
	case rep.Pending:
		fmt.Printf("%s passed the gate; making its golden snapshot %s...\n", rep.Version, rep.Snapshot)
	case rep.Candidate != "":
		fmt.Printf("not promoted; the new model is in %s (kling ai chispa eval -model %s -data <file>)\n", rep.Candidate, rep.Candidate)
	}
	if rep.Note != "" {
		fmt.Println(rep.Note)
	}
	if rep.Cascade != "" {
		fmt.Println(rep.Cascade)
	}
	if rep.Stored != "" {
		fmt.Printf("report in %s\n", rep.Stored)
	}
}

func aiRollback(args []string) error {
	fs := flag.NewFlagSet("ai rollback", flag.ExitOnError)
	to := fs.String("to", "", "version to serve (default: the previous one)")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling ai rollback <task> [-to vN]")
	}
	c, err := mk()
	if err != nil {
		return err
	}
	var vc aigw.VersionChange
	if err := c.do(http.MethodPost, "/v1/admin/rollback", aigw.RollbackRequest{Task: fs.Arg(0), To: *to}, &vc); err != nil {
		return err
	}
	where := vc.Written
	if vc.Snapshot != "" {
		where = "golden " + vc.Snapshot
	}
	fmt.Printf("task %s: %s -> %s (%s)\n", vc.Task, vc.From, vc.To, where)
	for _, n := range vc.Cascades {
		fmt.Println("  " + n)
	}
	return nil
}

// printLearn es la sección de mejora continua de `kling ai ls`.
func printLearn(tasks []aigw.TaskInfo) {
	var rows []aigw.TaskInfo
	for _, t := range tasks {
		if t.Learn != nil {
			rows = append(rows, t)
		}
	}
	if len(rows) == 0 {
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "\nLEARN TASK\tCAPTURE\tSERVING\tVERSIONS (held-out coverage / precision)\tCAPTURED\tTO REVIEW\tESCALATED (WINDOW)")
	for _, t := range rows {
		l := t.Learn
		var vs []string
		for _, v := range l.Versions {
			s := fmt.Sprintf("v%d", v.N)
			if v.Heldout != nil {
				s += fmt.Sprintf(" %.3f/%.3f", v.Heldout.Coverage, v.Heldout.ConfidentPrecision)
			}
			vs = append(vs, s)
		}
		if l.Pending != nil {
			vs = append(vs, fmt.Sprintf("v%d pending", l.Pending.N))
		}
		review := "?"
		if l.PendingReview >= 0 {
			review = fmt.Sprint(l.PendingReview)
		}
		win := "-"
		if l.WindowRequests > 0 {
			win = fmt.Sprintf("%.3f of %d (%d min)", l.WindowEscalationRate, l.WindowRequests, l.WindowMinutes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", t.Name, l.Capture, orDash(l.Version), orDash(strings.Join(vs, ", ")), l.Captured, review, win)
	}
	_ = tw.Flush()
	for _, t := range rows {
		if r := t.Learn.LastRetrain; r != nil {
			fmt.Printf("task %s: last retrain %s: %s\n", t.Name, r.At.Local().Format(time.DateTime), r.Verdict)
		}
	}
}
