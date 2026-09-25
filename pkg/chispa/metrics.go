package chispa

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Scored es una predicción ya hecha frente a su etiqueta correcta. Gold < 0
// significa que la etiqueta del ejemplo no existe en el modelo (cuenta como
// fallo). Lo usan la evaluación de un modelo y las líneas base, para que todos
// los números salgan de la misma cuenta.
type Scored struct {
	Gold, Pred int
	Prob       float64
	Confident  bool
}

// ClassReport son las métricas de una etiqueta.
type ClassReport struct {
	Label     string  `json:"label"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`   // ejemplos con esta etiqueta correcta
	Predicted int     `json:"predicted"` // veces que se predijo
	Threshold float64 `json:"threshold,omitempty"`
	// ConfidentN / ConfidentPrecision: predicciones de esta clase por encima de
	// τ y su precisión. Es el número que promete el umbral.
	ConfidentN         int     `json:"confident_n"`
	ConfidentPrecision float64 `json:"confident_precision"`
}

// Report es la evaluación completa. Las cuatro cifras de la cascada son
// Coverage, ConfidentPrecision, EscalatedN y ECE: cuánto contesta Chispa solo, con
// qué precisión, cuánto le pasa al modelo mayor y si sus probabilidades
// significan lo que dicen.
type Report struct {
	N          int           `json:"n"`
	Unknown    int           `json:"unknown_labels"`
	Accuracy   float64       `json:"accuracy"`
	MacroF1    float64       `json:"macro_f1"`
	WeightedF1 float64       `json:"weighted_f1"`
	Classes    []ClassReport `json:"classes"`
	// Confusion[g][p]: ejemplos de etiqueta g predichos como p.
	Confusion [][]int `json:"confusion"`
	HasProb   bool    `json:"has_prob"`
	ECE       float64 `json:"ece"`
	// Coverage: fracción contestada con confianza. ConfidentPrecision: su
	// exactitud. EscalatedAccuracy: lo que acertaría Chispa en lo que escala
	// (informativo: si es alto, los umbrales son demasiado prudentes).
	Coverage           float64  `json:"coverage"`
	ConfidentN         int      `json:"confident_n"`
	ConfidentPrecision float64  `json:"confident_precision"`
	EscalatedN         int      `json:"escalated_n"`
	EscalatedAccuracy  float64  `json:"escalated_accuracy"`
	Labels             []string `json:"labels"`
}

// Evaluate pasa el modelo por los ejemplos y calcula el informe.
func Evaluate(m *Model, exs []Example) *Report {
	rows := make([]Scored, len(exs))
	for i, ex := range exs {
		p := m.Predict(ex.Input())
		rows[i] = Scored{Gold: m.GoldIndex(ex.Label), Pred: p.Index, Prob: p.Prob, Confident: p.Confident}
	}
	return Score(m.Labels, rows, m.Thresholds, true)
}

// Score calcula el informe a partir de predicciones hechas. thresholds puede
// ser nil (una línea base sin umbrales); hasProb dice si Prob significa algo
// (si no, no hay ECE).
func Score(labels []string, rows []Scored, thresholds []float64, hasProb bool) *Report {
	L := len(labels)
	r := &Report{N: len(rows), HasProb: hasProb, Labels: labels, Confusion: make([][]int, L)}
	for i := range r.Confusion {
		r.Confusion[i] = make([]int, L)
	}
	cls := make([]ClassReport, L)
	confOK := make([]int, L)
	correct, confCorrect, escCorrect := 0, 0, 0
	const bins = 15
	var binN, binOK [bins]int
	var binConf [bins]float64
	for _, s := range rows {
		ok := s.Gold == s.Pred
		if s.Gold < 0 {
			r.Unknown++
		} else {
			cls[s.Gold].Support++
			r.Confusion[s.Gold][s.Pred]++
		}
		cls[s.Pred].Predicted++
		if ok {
			correct++
		}
		if s.Confident {
			r.ConfidentN++
			cls[s.Pred].ConfidentN++
			if ok {
				confCorrect++
				confOK[s.Pred]++
			}
		} else if ok {
			escCorrect++
		}
		if hasProb {
			b := min(int(s.Prob*bins), bins-1)
			b = max(b, 0)
			binN[b]++
			binConf[b] += s.Prob
			if ok {
				binOK[b]++
			}
		}
	}
	div := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}
	r.Accuracy = div(correct, r.N)
	r.Coverage = div(r.ConfidentN, r.N)
	r.ConfidentPrecision = div(confCorrect, r.ConfidentN)
	r.EscalatedN = r.N - r.ConfidentN
	r.EscalatedAccuracy = div(escCorrect, r.EscalatedN)
	present, supp := 0, 0
	for c := range cls {
		cr := &cls[c]
		cr.Label = labels[c]
		tp := r.Confusion[c][c]
		cr.Precision = div(tp, cr.Predicted)
		cr.Recall = div(tp, cr.Support)
		if cr.Precision+cr.Recall > 0 {
			cr.F1 = 2 * cr.Precision * cr.Recall / (cr.Precision + cr.Recall)
		}
		cr.ConfidentPrecision = div(confOK[c], cr.ConfidentN)
		if thresholds != nil {
			cr.Threshold = thresholds[c]
		}
		// Macro-F1 sobre las clases presentes en el conjunto (o predichas): una
		// clase que no aparece ni se predice no es ni acierto ni fallo.
		if cr.Support > 0 || cr.Predicted > 0 {
			present++
			r.MacroF1 += cr.F1
		}
		r.WeightedF1 += cr.F1 * float64(cr.Support)
		supp += cr.Support
	}
	if present > 0 {
		r.MacroF1 /= float64(present)
	}
	if supp > 0 {
		r.WeightedF1 /= float64(supp)
	}
	if hasProb {
		for b := 0; b < bins; b++ {
			if binN[b] > 0 {
				gap := div(binOK[b], binN[b]) - binConf[b]/float64(binN[b])
				if gap < 0 {
					gap = -gap
				}
				r.ECE += gap * float64(binN[b]) / float64(r.N)
			}
		}
	}
	r.Classes = cls
	return r
}

// WriteText imprime el informe para una persona (la CLI).
func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "examples: %d", r.N)
	if r.Unknown > 0 {
		fmt.Fprintf(w, " (%d with a label the model does not know)", r.Unknown)
	}
	fmt.Fprintf(w, "\naccuracy: %.3f   macro-F1: %.3f   weighted-F1: %.3f", r.Accuracy, r.MacroF1, r.WeightedF1)
	if r.HasProb {
		fmt.Fprintf(w, "   ECE: %.3f", r.ECE)
	}
	fmt.Fprintf(w, "\nconfident: %.1f%% of inputs (%d), precision on confident: %.3f\n",
		100*r.Coverage, r.ConfidentN, r.ConfidentPrecision)
	fmt.Fprintf(w, "escalated: %.1f%% (%d), accuracy there if answered anyway: %.3f\n\n",
		100*(1-r.Coverage), r.EscalatedN, r.EscalatedAccuracy)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "label\tprecision\trecall\tF1\tsupport\tτ\tconfident\tconf-prec\t")
	for _, c := range r.Classes {
		tau := "-"
		if c.Threshold >= NeverConfident {
			tau = "never"
		} else if c.Threshold > 0 {
			tau = fmt.Sprintf("%.3f", c.Threshold)
		}
		fmt.Fprintf(tw, "%s\t%.3f\t%.3f\t%.3f\t%d\t%s\t%d\t%.3f\t\n",
			c.Label, c.Precision, c.Recall, c.F1, c.Support, tau, c.ConfidentN, c.ConfidentPrecision)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nconfusion (rows = true label, columns = predicted):")
	tw = tabwriter.NewWriter(w, 0, 0, 1, ' ', tabwriter.AlignRight)
	head := []string{""}
	for _, l := range r.Labels {
		head = append(head, abbrev(l))
	}
	fmt.Fprintln(tw, strings.Join(head, "\t")+"\t")
	for g, row := range r.Confusion {
		cells := []string{r.Labels[g]}
		for _, v := range row {
			cells = append(cells, fmt.Sprint(v))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t")+"\t")
	}
	tw.Flush()
}

func abbrev(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
