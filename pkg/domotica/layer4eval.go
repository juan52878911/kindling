package domotica

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// EVALUACIÓN DE LA CAPA 4.
//
// La pregunta no es «¿acierta VON?» sino «¿es mejor preguntarle que no hacer
// nada?»: sin la capa 4, lo que las capas rápidas escalan se queda sin
// ejecutar, y eso acierta siempre que la frase no pedía nada (fuera de ámbito).
// Así que se compara, en las filas escaladas, VON contra «escalar y ya» con la
// prueba de McNemar (como `kling ai eval`), y se cuentan aparte los errores que
// VON añade: actuar donde no había que hacer nada, o hacer otra cosa.

// multiGold es el oro completo de las frases de reto con varias órdenes (el
// JSONL solo guarda la primera, que es lo que miden las capas rápidas).
var multiGold = map[string][]Action{
	"enciende la luz y baja la persiana":                           {act("turn_on", DevLight, "", nil, ""), act("cover_close", DevBlinds, "", nil, "")},
	"apaga la tele y enciende la luz del salón":                    {act("turn_off", DevTV, "", nil, ""), act("turn_on", DevLight, "living_room", nil, "")},
	"sube la temperatura y cierra las persianas":                   {act("temperature_up", DevThermostat, "", nil, ""), act("cover_close", DevBlinds, "", nil, "")},
	"cierra la puerta con llave y activa la alarma":                {act("lock", DevLock, "", nil, ""), act("alarm_arm", DevAlarm, "", nil, "")},
	"pon la luz al cincuenta por ciento y luego pausa la película": {act("set_brightness", DevLight, "", ptr(50), ""), act("media_pause", "", "", nil, "")},
	"turn off the lights and lock the door":                        {act("turn_off", DevLight, "", nil, ""), act("lock", DevLock, "", nil, "")},
	"open the blinds and turn on the fan":                          {act("cover_open", DevBlinds, "", nil, ""), act("turn_on", DevFan, "", nil, "")},
	"pause the tv then dim the lights":                             {act("media_pause", DevTV, "", nil, ""), act("brightness_down", DevLight, "", nil, "")},
	"set the heating to 21 degrees and close the curtains":         {act("set_temperature", DevThermostat, "", ptr(21), ""), act("cover_close", DevBlinds, "", nil, "")},
}

func ptr(v float64) *float64 { return &v }

func act(intent, device, area string, value *float64, color string) Action {
	s := Slots{Device: device, Area: area, Color: color}
	if value != nil {
		s.Value, s.HasValue = *value, true
	}
	return Action{Intent: intent, Slots: Resolve(intent, s)}
}

// GoldActions es lo que habría que ejecutar para una fila.
func GoldActions(r Row) []Action {
	if r.Intent == OutOfScope {
		return nil
	}
	if g, ok := multiGold[r.Text]; ok && r.Class == "multi" {
		return g
	}
	return []Action{{Intent: r.Intent, Slots: Resolve(r.Intent, r.Slots)}}
}

// ActionsMatch compara dos listas de acciones sin mirar el orden. En lo
// multimedia sin dispositivo en el oro («pausa») cualquier reproductor vale:
// cuál suena lo decide el estado, no la frase.
func ActionsMatch(gold, pred []Action) bool {
	if len(gold) != len(pred) {
		return false
	}
	used := make([]bool, len(pred))
	for _, g := range gold {
		found := false
		for j, p := range pred {
			if !used[j] && actionEq(g, p) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func actionEq(g, p Action) bool {
	if g.Intent != p.Intent {
		return false
	}
	gs, ps := Resolve(g.Intent, g.Slots), Resolve(p.Intent, p.Slots)
	if it := Intent(g.Intent); it != nil && it.Device == "" && gs.Device == "" {
		ps.Device = ""
	}
	return SlotsEqual(gs, ps)
}

// L4Group son las cuentas de un grupo de filas.
type L4Group struct {
	N         int     // filas evaluadas
	Weight    float64 // cuántas filas reales representa cada una (muestreo)
	FastRight int     // la cascada rápida acierta con confianza (no escala)
	FastWrong int     // la cascada rápida se equivoca con confianza
	Escalated int
	// En las escaladas:
	NothingOK int // «no hacer nada» era lo correcto
	VONOK     int // VON acierta la orden completa (o no hace nada cuando no había que hacer nada)
	VONWrong  int // VON actúa y se equivoca (acción de más o distinta): error confiado nuevo
	VONQuiet  int // VON no actúa (vacío o inválido) y había que actuar
	VONBad    int // salida inválida (se trata como no hacer nada + aclaración)
	VONErr    int // la llamada falló
	Win, Loss int // discrepancias con «no hacer nada»: VON bien y nada mal / al revés
}

// Layer4Report es la evaluación de la capa 4.
type Layer4Report struct {
	Model    string
	PromptID string
	Groups   map[string]*L4Group
	Latency  []float64  // ms por llamada a VON, ordenadas
	Errors   []string   // muestras de errores de VON para leer a mano
	Rows     []L4Result // una por fila escalada (para `-dump`)
}

// L4Row es una fila con su grupo y su peso.
type L4Row struct {
	Row    Row
	Group  string
	Weight float64
}

// EvaluateLayer4 pasa las filas por las capas 1–3 (fast) y, lo que escala,
// por llm. concurrency > 1 usa varias réplicas si el gateway las tiene.
func EvaluateLayer4(ctx context.Context, fast FastFunc, llm Layer, rows []L4Row, concurrency int) (*Layer4Report, error) {
	rep := &Layer4Report{PromptID: PromptID(), Groups: map[string]*L4Group{}}
	type res struct {
		i   int
		d   Decision
		err error
		ms  float64
	}
	fastD := make([]Decision, len(rows))
	var todo []int
	for i, r := range rows {
		d, err := fast(ctx, r.Row.Text, r.Row.Lang)
		if err != nil {
			return nil, fmt.Errorf("fast layers on %q: %w", r.Row.Text, err)
		}
		if d.Lang == "" {
			d.Lang = r.Row.Lang
		}
		fastD[i] = d
		if !fastD[i].Confident {
			todo = append(todo, i)
		}
	}
	if concurrency < 1 {
		concurrency = 1
	}
	out := make([]res, len(rows))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				t0 := time.Now()
				d, err := llm.Decide(WithPrev(ctx, fastD[i]), rows[i].Row.Text, fastD[i].Lang)
				out[i] = res{i: i, d: d, err: err, ms: float64(time.Since(t0).Microseconds()) / 1e3}
			}
		}()
	}
	for _, i := range todo {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()

	for i, r := range rows {
		g := rep.Groups[r.Group]
		if g == nil {
			g = &L4Group{Weight: r.Weight}
			rep.Groups[r.Group] = g
		}
		g.N++
		gold := GoldActions(r.Row)
		fd := fastD[i]
		if fd.Confident {
			if ActionsMatch(gold, fd.ActionList()) {
				g.FastRight++
			} else {
				g.FastWrong++
			}
			continue
		}
		g.Escalated++
		nothingOK := len(gold) == 0
		if nothingOK {
			g.NothingOK++
		}
		o := out[i]
		if o.err != nil {
			g.VONErr++
			rep.Rows = append(rep.Rows, L4Result{Group: r.Group, Text: r.Row.Text, Gold: nonNil(gold), Pred: []Action{}, OK: nothingOK,
				FastReason: fd.Reason, Err: o.err.Error()})
			if len(rep.Errors) < maxL4Errors {
				rep.Errors = append(rep.Errors, fmt.Sprintf("[%s] %q: error: %v", r.Group, r.Row.Text, o.err))
			}
			continue
		}
		rep.Latency = append(rep.Latency, o.ms)
		if rep.Model == "" {
			rep.Model = o.d.Model
		}
		pred := o.d.ActionList()
		ok := ActionsMatch(gold, pred)
		rep.Rows = append(rep.Rows, L4Result{Group: r.Group, Text: r.Row.Text, Gold: nonNil(gold), Pred: nonNil(pred), OK: ok,
			Kind: o.d.Kind, Reason: o.d.Reason, FastReason: fd.Reason, CommandVerb: hasCommandVerb(r.Row.Text), MS: o.ms})
		switch {
		case ok:
			g.VONOK++
		case len(pred) > 0:
			g.VONWrong++
		default:
			g.VONQuiet++
		}
		if o.d.Reason == ReasonInvalidOutput {
			g.VONBad++
		}
		switch {
		case ok && !nothingOK:
			g.Win++
		case !ok && nothingOK:
			g.Loss++
		}
		if !ok && len(rep.Errors) < maxL4Errors {
			raw := ""
			if o.d.LLM != nil {
				raw = o.d.LLM.Raw
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("[%s] %q gold=%s pred=%s kind=%s reason=%s raw=%s", r.Group, r.Row.Text, fmtActions(gold), fmtActions(pred), o.d.Kind, o.d.Reason, compactJSON(raw)))
		}
	}
	sort.Float64s(rep.Latency)
	return rep, ctx.Err()
}

func fmtActions(as []Action) string {
	if len(as) == 0 {
		return "[]"
	}
	s := "["
	for i, a := range as {
		if i > 0 {
			s += " "
		}
		s += a.Intent + fmt.Sprint(a.Slots.Pairs())
	}
	return s + "]"
}

// Total suma los grupos con su peso: la cascada entera con y sin capa 4.
type L4Total struct {
	N                       float64 // filas (ponderadas)
	WithoutExact, WithExact float64 // orden completa bien (sin acciones de más)
	WithoutWrong, WithWrong float64 // errores confiados: la habitación haría algo que no se pidió
}

// Alcances de la capa 4: a qué escaladas contesta.
const (
	// ScopeAll: a todo lo que escala, también a lo que Chispa da por «fuera de
	// ámbito» (ahí está lo indirecto), con el veto de VON.Decide.
	ScopeAll = "all"
	// ScopeUncertain: solo a lo que Chispa duda (probabilidad baja, varias
	// órdenes, falta un valor). Lo que Chispa da por fuera de ámbito se queda sin
	// hacer, sin llamar al LLM: lo indirecto lo tendrá que decidir la capa 3.
	ScopeUncertain = "uncertain"
)

// InScope dice si la capa 4 contesta a una escalada con ese motivo.
func InScope(scope, fastReason string) bool {
	return scope != ScopeUncertain || fastReason != ReasonOutOfScope
}

// Total pondera los grupos (nil = todos) con la capa 4 en un alcance.
func (r *Layer4Report) Total(groups []string, scope string) L4Total {
	var t L4Total
	for name, g := range r.Groups {
		if groups != nil && !contains(groups, name) {
			continue
		}
		w := weight(g)
		t.N += w * float64(g.N)
		t.WithoutExact += w * float64(g.FastRight+g.NothingOK)
		t.WithExact += w * float64(g.FastRight)
		t.WithoutWrong += w * float64(g.FastWrong)
		t.WithWrong += w * float64(g.FastWrong)
	}
	for _, row := range r.Rows {
		g := r.Groups[row.Group]
		if g == nil || (groups != nil && !contains(groups, row.Group)) {
			continue
		}
		ok, acted := row.under(scope)
		if ok {
			t.WithExact += weight(g)
		}
		if acted && !ok {
			t.WithWrong += weight(g)
		}
	}
	return t
}

// under: si la fila acierta y si la habitación actúa con la capa 4 en scope.
func (row L4Result) under(scope string) (ok, acted bool) {
	if !InScope(scope, row.FastReason) {
		return len(row.Gold) == 0, false
	}
	return row.OK, len(row.Pred) > 0
}

func weight(g *L4Group) float64 {
	if g.Weight <= 0 {
		return 1
	}
	return g.Weight
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Gate es el veredicto: la capa 4 se activa si (1) en lo escalado gana a «no
// hacer nada» con McNemar de una cola p < 0,05 y (2) la cascada entera,
// ponderada como los datos reales, acierta más con ella que sin ella. La
// segunda condición es la que pesa los errores confiados nuevos: en MASSIVE
// hay 12 frases fuera de ámbito por cada orden, y actuar en un 2 % de ellas se
// come lo que se gana en las órdenes.
type Gate struct {
	Scope     string
	Win, Loss int
	P         float64
	Total     L4Total
	Pass      bool
	Why       string
}

// Gate calcula el veredicto sobre los grupos dados (nil = todos) con la capa
// 4 en un alcance.
func (r *Layer4Report) Gate(groups []string, scope string) Gate {
	g := Gate{Scope: scope, Total: r.Total(groups, scope)}
	for _, row := range r.Rows {
		if groups != nil && !contains(groups, row.Group) {
			continue
		}
		ok, _ := row.under(scope)
		nothing := len(row.Gold) == 0
		switch {
		case ok && !nothing:
			g.Win++
		case !ok && nothing:
			g.Loss++
		}
	}
	g.P = mcnemarOneSided(g.Win, g.Loss)
	switch {
	case g.P >= 0.05:
		g.Why = fmt.Sprintf("does not beat doing nothing on escalated rows (wins %d, losses %d, McNemar p=%.3g)", g.Win, g.Loss, g.P)
	case g.Total.WithExact <= g.Total.WithoutExact:
		g.Why = fmt.Sprintf("the weighted cascade is not better with it (%.3f vs %.3f without)", ratioF(g.Total.WithExact, g.Total.N), ratioF(g.Total.WithoutExact, g.Total.N))
	default:
		g.Pass = true
		g.Why = fmt.Sprintf("beats doing nothing (wins %d, losses %d, p=%.3g) and the weighted cascade goes %.3f → %.3f",
			g.Win, g.Loss, g.P, ratioF(g.Total.WithoutExact, g.Total.N), ratioF(g.Total.WithExact, g.Total.N))
	}
	return g
}

func ratioF(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// mcnemarOneSided: P(X ≥ win) con X ~ Bin(win+loss, 1/2), exacta.
func mcnemarOneSided(win, loss int) float64 {
	n := win + loss
	if n == 0 {
		return 1
	}
	p := 0.0
	for k := win; k <= n; k++ {
		lg, _ := math.Lgamma(float64(n + 1))
		a, _ := math.Lgamma(float64(k + 1))
		b, _ := math.Lgamma(float64(n - k + 1))
		p += math.Exp(lg - a - b - float64(n)*math.Ln2)
	}
	return math.Min(1, p)
}

// Percentile de la latencia de VON en ms.
func (r *Layer4Report) Percentile(p float64) float64 {
	if len(r.Latency) == 0 {
		return 0
	}
	return r.Latency[int(p*float64(len(r.Latency)-1))]
}

// WriteLayer4 escribe la tabla por grupo.
func WriteLayer4(w io.Writer, r *Layer4Report) {
	names := make([]string, 0, len(r.Groups))
	for n := range r.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "%-26s %5s %6s %5s | %8s %7s %6s %6s %5s %5s | %4s %4s\n",
		"group", "n", "weight", "esc", "nothing", "von", "wrong", "quiet", "bad", "err", "win", "loss")
	for _, n := range names {
		g := r.Groups[n]
		fmt.Fprintf(w, "%-26s %5d %6.1f %5d | %8.3f %7.3f %6d %6d %5d %5d | %4d %4d\n", n, g.N, g.Weight, g.Escalated,
			ratio(g.NothingOK, g.Escalated), ratio(g.VONOK, g.Escalated), g.VONWrong, g.VONQuiet, g.VONBad, g.VONErr, g.Win, g.Loss)
	}
}

// L4Result es una fila escalada con lo que hizo VON.
type L4Result struct {
	Group       string   `json:"group"`
	Text        string   `json:"text"`
	Gold        []Action `json:"gold"`
	Pred        []Action `json:"pred"`
	OK          bool     `json:"ok"`
	Kind        string   `json:"kind,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	FastReason  string   `json:"fast_reason"`
	CommandVerb bool     `json:"command_verb"`
	Err         string   `json:"error,omitempty"`
	MS          float64  `json:"ms"`
}

func nonNil(a []Action) []Action {
	if a == nil {
		return []Action{}
	}
	return a
}

// maxL4Errors: errores de muestra que guarda la evaluación.
const maxL4Errors = 500

// compactJSON quita saltos de línea y sangrías para leer los errores en una
// línea.
func compactJSON(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
