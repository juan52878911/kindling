package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/examples/ci-triage/triage"
)

// maxFeedbackBytes acota el fichero de confirmaciones (unas 20 000).
const maxFeedbackBytes = 64 << 20

// gwFlags son las banderas comunes para hablar con el gateway.
type gwFlags struct {
	addr, tokenFile             string
	linesTask, catTask, sumTask string
	von                         string
	timeout                     time.Duration
}

func gatewayFlags(fs *flag.FlagSet) *gwFlags {
	dir := klingDir()
	sock := os.Getenv("KLING_AI_SOCKET")
	if sock == "" {
		sock = filepath.Join(dir, "ai.sock")
	}
	g := &gwFlags{}
	fs.StringVar(&g.addr, "gateway", sock, "the AI gateway (`kling ai serve`): its Unix socket or http://host:port")
	fs.StringVar(&g.tokenFile, "token-file", filepath.Join(dir, "ai.token"), "gateway token for http:// ($KLING_AI_TOKEN wins)")
	fs.StringVar(&g.linesTask, "lines-task", triage.DefaultOptions.LinesTask, "gateway task that scores each line (Chispa, explains/noise)")
	fs.StringVar(&g.catTask, "category-task", triage.DefaultOptions.CategoryTask, "gateway task that categorizes the chunk (Chispa)")
	fs.StringVar(&g.sumTask, "summary-task", triage.DefaultOptions.SummaryTask, "gateway generation task for VON (empty: no VON)")
	fs.StringVar(&g.von, "von-when", triage.DefaultOptions.VON, "when to ask VON: escalated (Chispa doubts the category), always or off")
	fs.DurationVar(&g.timeout, "timeout", 2*time.Minute, "deadline of one gateway request (VON may have to wake up)")
	return g
}

// klingDir es donde `kling ai serve` deja su socket y su token: junto a su
// config.json ($KLING_CONFIG, o ~/.config/kling también en macOS).
func klingDir() string {
	if p := os.Getenv("KLING_CONFIG"); p != "" {
		return filepath.Dir(p)
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "kling")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "kling")
}

func (g *gwFlags) dial(conns int) (*triage.Gateway, error) {
	return triage.NewGateway(g.addr, g.tokenFile, g.timeout, conns)
}

func (g *gwFlags) options() triage.Options {
	o := triage.DefaultOptions
	o.LinesTask, o.CategoryTask, o.SummaryTask, o.VON = g.linesTask, g.catTask, g.sumTask, g.von
	return o
}

func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	gw := gatewayFlags(fs)
	workers := fs.Int("workers", 8, "classify requests in flight")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	confirm := fs.String("confirm", "", "record the right category for this log in -feedback: \"ok\" accepts the result, or a category ("+strings.Join(triage.HumanCategories, ", ")+")")
	note := fs.String("note", "", "with -confirm: a short note for the record")
	fbPath := fs.String("feedback", "ci-triage-feedback.jsonl", "with -confirm: JSONL file the confirmations are appended to (trainable by `kling ai chispa train`)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: ci-triage analyze [flags] <logfile|->")
	}
	switch gw.von {
	case "escalated", "always", "off":
	default:
		return fmt.Errorf("-von-when must be escalated, always or off")
	}
	t0 := time.Now()
	var lg *triage.Log
	var err error
	if p := fs.Arg(0); p == "-" {
		lg, err = triage.Read(os.Stdin, triage.DefaultLimits)
	} else {
		lg, err = triage.ReadFile(p, triage.DefaultLimits)
	}
	if err != nil {
		return err
	}
	read := ms(time.Since(t0))
	g, err := gw.dial(*workers)
	if err != nil {
		return err
	}
	o := gw.options()
	o.Workers = *workers
	res, err := triage.Analyze(context.Background(), g, lg, o)
	if err != nil {
		return err
	}
	res.Timing.Read = read
	res.Timing.Total += read
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		printResult(res)
	}
	if *confirm == "" {
		return nil
	}
	label := *confirm
	if label == "ok" {
		label = res.Category
	}
	fb, err := triage.NewFeedback(res, res.Fields, triage.HashLog(lg), label, *note)
	if err != nil {
		return err
	}
	fl, err := triage.NewFeedbackLog(*fbPath, maxFeedbackBytes)
	if err != nil {
		return err
	}
	if err := fl.Append(fb); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recorded %s (agreed: %v) in %s\n", label, fb.Agreed, *fbPath)
	return nil
}

func printResult(r *triage.Result) {
	tr := ""
	if r.Truncated {
		tr = ", truncated: only the tail was read"
	}
	fmt.Printf("log: %d lines (%s%s), %d classified by Chispa\n\n", r.Lines, r.Format, tr, r.Scored)
	if len(r.Chunks) == 0 {
		fmt.Println("no line looks like it explains a failure")
	}
	for _, c := range r.Chunks {
		fmt.Printf("lines %d-%d (score %.2f)\n", c.From, c.To, c.Score)
	}
	fmt.Println()
	for l := range strings.SplitSeq(strings.TrimRight(r.Chunk, "\n"), "\n") {
		fmt.Println("  │ " + l)
	}
	fmt.Println()
	conf := "confident"
	if !r.Confident {
		conf = "unsure"
	}
	fmt.Printf("category: %s  (decided by %s; Chispa %s, p=%.2f", r.Category, r.Layer, conf, r.Prob)
	if len(r.Candidates) > 1 {
		var cs []string
		for _, c := range r.Candidates {
			cs = append(cs, fmt.Sprintf("%s %.2f", c.Label, c.Prob))
		}
		fmt.Printf("; candidates %s", strings.Join(cs, ", "))
	}
	fmt.Println(")")
	if r.VON != nil {
		fmt.Printf("summary:   %s\nnext step: %s\n", r.VON.Summary, r.VON.NextStep)
		fmt.Printf("           (VON %s said %s: %d prompt + %d generated tokens)\n", r.VON.Model, r.VON.Category, r.VON.PromptTokens, r.VON.CompletionTokens)
	}
	if r.VONError != "" {
		fmt.Printf("VON did not answer: %s\n", r.VONError)
	}
	t := r.Timing
	fmt.Printf("\nlatency: read %.1f ms · features %.1f ms · Chispa on %d lines %.1f ms (%.2f ms inside the gateway) · chunk %.2f ms · category %.2f ms",
		t.Read, t.Features, r.Scored, t.Lines, t.LinesIn, t.Locate, t.Category)
	if t.VON > 0 {
		fmt.Printf(" · VON %.0f ms", t.VON)
	}
	fmt.Printf(" · total %.1f ms\n", t.Total)
}
