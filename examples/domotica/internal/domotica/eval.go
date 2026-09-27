package domotica

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Frases de reto escritas a mano (del proyecto, no de las fuentes): lenguaje
// indirecto, varias órdenes a la vez, casi-fuera-de-ámbito y paráfrasis
// directas. Miden lo que las capas rápidas NO deben contestar con confianza.
//
//go:embed challenge.jsonl
var challengeData []byte

// Challenge devuelve las frases de reto.
func Challenge() []Row {
	rows, err := ReadRows(bytes.NewReader(challengeData))
	if err != nil {
		panic("domotica: challenge.jsonl: " + err.Error())
	}
	return rows
}

// Órdenes indirectas escritas a mano para la capa 3 («estoy tiritando» →
// subir la temperatura): 9 intenciones × 2 idiomas × 9 frases, repartidas
// 5/2/2 en train/valid/test (campo split). Los datos de las fuentes casi no
// traen lenguaje indirecto, y un codificador congelado solo aprende de él con
// ejemplos (el pocos-ejemplos de SetFit). Ninguna frase coincide con las de
// reto, y se escribieron evitando sus palabras clave; aun así son del mismo
// proyecto: docs/codificador.md dice qué cifras dependen de ellas.
//
//go:embed indirect.jsonl
var indirectData []byte

// Indirect devuelve las órdenes indirectas de un reparto (train, valid, test;
// "" = todas), con Class "indirect".
func Indirect(split string) []Row {
	rows, err := ReadRows(bytes.NewReader(indirectData))
	if err != nil {
		panic("domotica: indirect.jsonl: " + err.Error())
	}
	out := rows[:0]
	for _, r := range rows {
		if split == "" || r.Split == split {
			r.Class = "indirect"
			out = append(out, r)
		}
	}
	return out
}

// System es una forma de decidir que se evalúa.
type System func(text, lang string) Decision

// GroupScore son las cuentas de un grupo de frases (un idioma, una fuente…).
type GroupScore struct {
	N               int
	IntentOK        int // intención correcta
	Exact           int // intención y todos los huecos correctos
	Confident       int
	ConfidentExact  int
	ConfidentIntent int
	SlotTP, SlotFP  int
	SlotFN          int
	tp, fp, fn      map[string]int
}

func newGroup() *GroupScore {
	return &GroupScore{tp: map[string]int{}, fp: map[string]int{}, fn: map[string]int{}}
}

// Métricas derivadas.
func (g *GroupScore) IntentAcc() float64     { return ratio(g.IntentOK, g.N) }
func (g *GroupScore) ExactAcc() float64      { return ratio(g.Exact, g.N) }
func (g *GroupScore) Coverage() float64      { return ratio(g.Confident, g.N) }
func (g *GroupScore) PrecisionConf() float64 { return ratio(g.ConfidentExact, g.Confident) }

// SlotF1 es el F1 de los pares hueco=valor (normalizados) en las frases de
// oro dentro de ámbito.
func (g *GroupScore) SlotF1() float64 {
	_, _, f := prf(g.SlotTP, g.SlotFP, g.SlotFN)
	return f
}

// MacroF1 promedia el F1 de las intenciones presentes en el oro del grupo.
func (g *GroupScore) MacroF1() float64 {
	sum, n := 0.0, 0
	for l := range g.tp {
		if g.tp[l]+g.fn[l] == 0 {
			continue
		}
		_, _, f := prf(g.tp[l], g.fp[l], g.fn[l])
		sum += f
		n++
	}
	for l, v := range g.fn {
		if _, seen := g.tp[l]; !seen && v > 0 {
			n++ // intención de oro nunca acertada: F1 0
		}
	}
	return ratio2(sum, n)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func ratio2(a float64, b int) float64 {
	if b == 0 {
		return 0
	}
	return a / float64(b)
}

func prf(tp, fp, fn int) (p, r, f float64) {
	p, r = ratio(tp, tp+fp), ratio(tp, tp+fn)
	if p+r > 0 {
		f = 2 * p * r / (p + r)
	}
	return
}

// Report es la evaluación de un sistema.
type Report struct {
	Name    string
	Groups  map[string]*GroupScore
	Latency []float64 // µs por frase, ordenadas al terminar
	Layers  map[string]int
	// Errores de muestra (pocos) para leer a mano.
	Errors []string
}

// SlotsEqual compara huecos resueltos.
func SlotsEqual(a, b Slots) bool {
	return strings.Join(a.Pairs(), ",") == strings.Join(b.Pairs(), ",")
}

// Evaluate pasa rows por sys. Cada fila cuenta en «all», en su idioma, en su
// fuente y en fuente/idioma.
func Evaluate(name string, sys System, rows []Row) *Report {
	r := &Report{Name: name, Groups: map[string]*GroupScore{}, Layers: map[string]int{}}
	for _, row := range rows {
		d := sys(row.Text, row.Lang)
		r.Latency = append(r.Latency, d.LatencyUS)
		r.Layers[d.Layer]++
		gold := Resolve(row.Intent, row.Slots)
		pred := Resolve(d.Intent, d.Slots)
		intentOK := d.Intent == row.Intent
		exact := intentOK && SlotsEqual(gold, pred)
		if !exact && len(r.Errors) < 40 && d.Confident {
			r.Errors = append(r.Errors, fmt.Sprintf("[%s] %q gold=%s%v pred=%s%v", row.Source, row.Text, row.Intent, gold.Pairs(), d.Intent, pred.Pairs()))
		}
		scope := "inscope"
		if row.Intent == OutOfScope {
			scope = "oos"
		}
		for _, key := range []string{"all", row.Lang, row.Source, row.Source + "/" + row.Lang, scope, scope + "/" + row.Lang,
			scope + "/" + row.Source, scope + "/" + row.Source + "/" + row.Lang} {
			g := r.Groups[key]
			if g == nil {
				g = newGroup()
				r.Groups[key] = g
			}
			g.N++
			if intentOK {
				g.IntentOK++
				g.tp[row.Intent]++
			} else {
				g.fn[row.Intent]++
				if d.Intent != "" {
					g.fp[d.Intent]++
				}
			}
			if exact {
				g.Exact++
			}
			if d.Confident {
				g.Confident++
				if exact {
					g.ConfidentExact++
				}
				if intentOK {
					g.ConfidentIntent++
				}
			}
			if row.Intent != OutOfScope {
				gp := map[string]int{}
				for _, p := range gold.Pairs() {
					gp[p]++
				}
				if d.Intent != OutOfScope {
					for _, p := range pred.Pairs() {
						if gp[p] > 0 {
							gp[p]--
							g.SlotTP++
						} else {
							g.SlotFP++
						}
					}
				}
				for _, n := range gp {
					g.SlotFN += n
				}
			}
		}
	}
	sort.Float64s(r.Latency)
	return r
}

// Percentile de la latencia en µs (p en [0, 1]).
func (r *Report) Percentile(p float64) float64 {
	if len(r.Latency) == 0 {
		return 0
	}
	i := int(p * float64(len(r.Latency)-1))
	return r.Latency[i]
}

// WriteTable escribe una fila por grupo.
func WriteTable(w io.Writer, reports []*Report, groups []string) {
	fmt.Fprintf(w, "%-22s %-12s %6s %7s %7s %7s %7s %8s %8s\n", "system", "group", "n", "intent", "mF1", "slotF1", "exact", "cover", "prec@c")
	for _, rep := range reports {
		for _, gname := range groups {
			g := rep.Groups[gname]
			if g == nil {
				continue
			}
			fmt.Fprintf(w, "%-22s %-12s %6d %7.3f %7.3f %7.3f %7.3f %7.1f%% %8.3f\n", rep.Name, gname, g.N,
				g.IntentAcc(), g.MacroF1(), g.SlotF1(), g.ExactAcc(), 100*g.Coverage(), g.PrecisionConf())
		}
	}
}

// ChallengeScore resume el comportamiento en las frases de reto por clase:
// acierto confiado (bien), escalado (aceptable: lo decidirá una capa mayor) y
// error confiado (lo único grave: la habitación haría otra cosa).
type ChallengeScore struct {
	N, ConfidentRight, Escalated, ConfidentWrong int
}

// EvaluateChallenge puntúa sys en las frases de reto.
func EvaluateChallenge(sys System, rows []Row) (map[string]*ChallengeScore, []string) {
	out := map[string]*ChallengeScore{}
	var wrong []string
	for _, row := range rows {
		d := sys(row.Text, row.Lang)
		c := out[row.Class]
		if c == nil {
			c = &ChallengeScore{}
			out[row.Class] = c
		}
		c.N++
		exact := d.Intent == row.Intent && SlotsEqual(Resolve(row.Intent, row.Slots), Resolve(d.Intent, d.Slots))
		switch {
		case !d.Confident:
			c.Escalated++
		case exact:
			c.ConfidentRight++
		default:
			c.ConfidentWrong++
			wrong = append(wrong, fmt.Sprintf("[%s] %q → %s %v (%s)", row.Class, row.Text, d.Intent, d.Slots.Pairs(), d.Layer))
		}
	}
	return out, wrong
}
