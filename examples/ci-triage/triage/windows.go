package triage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// El análisis normal se queda con la cola del log (DefaultLimits): lo que
// explica un fallo casi siempre está al final. Casi: en una matriz de trabajos
// o en un log que sigue imprimiendo tras el fallo, el tramo bueno puede estar a
// mitad y la cola explica otra cosa. Por ventanas se puntúa el log ENTERO con
// memoria acotada: se recorre dos veces (una para contar las líneas, que la
// posición y la distancia al final son del log entero), se puntúa ventana a
// ventana, cada ventana propone sus mejores tramos y al final se eligen los
// mejores de todo el log con las mismas reglas que Locate. Chispa cuesta ~µs
// por línea: 344 000 líneas son unos segundos por el gateway.

// WindowOptions dice cómo se recorre un log por ventanas.
type WindowOptions struct {
	// Lines son las líneas puntuadas por ventana: lo que se tiene en memoria
	// a la vez (con líneas de 2 KiB como mucho).
	Lines int
	// Context son las líneas de más a cada lado de la ventana: para las
	// vecinas y las distancias de las características, para que un tramo
	// pueda crecer más allá del borde y para buscar el comando de Travis
	// detrás del tramo (CategoryFields mira 400 líneas).
	Context int
	// MaxBytes es lo que se lee del fichero; si pesa más, la cola (con aviso).
	MaxBytes int64
}

// DefaultWindowOptions: ventanas de 25 000 líneas (50 MiB en el peor caso) y
// hasta 1 GiB de log.
var DefaultWindowOptions = WindowOptions{Lines: 25_000, Context: 400, MaxBytes: 1 << 30}

// candidate es un tramo propuesto por una ventana, en índices del log entero,
// con sus líneas copiadas (y las que lo siguen, para CategoryFields): la
// ventana se tira en cuanto se puntúa.
type candidate struct {
	Chunk
	lines []Line
}

// AnalyzeWindows hace el triaje de un fichero de log entero, por ventanas.
// Necesita un fichero normal: lo lee dos veces.
func AnalyzeWindows(ctx context.Context, g *Gateway, path string, lim Limits, wo WindowOptions, o Options) (*Result, error) {
	if wo.Lines <= 0 || wo.Context < 0 || wo.MaxBytes <= 0 {
		return nil, errors.New("triage: window options must be positive")
	}
	t0 := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: scoring by windows needs a regular file (it is read twice)", path)
	}

	// Lo que no se lee: solo si pesa más que wo.MaxBytes.
	lg := &Log{}
	var start int64
	if st.Size() > wo.MaxBytes {
		start = st.Size() - wo.MaxBytes
		head, atLeast, err := skipHead(f, start, streamLimit(lim))
		if err != nil {
			return nil, err
		}
		lg.Truncated, lg.Skipped, lg.DroppedLines, lg.DroppedBytes, lg.DroppedAtLeast = true, start, head, start, atLeast
	}
	src := func() io.Reader { return io.LimitReader(f, st.Size()-start) }

	// Primera pasada: cuántas líneas hay.
	total := 0
	format, first, err := scanLines(src(), lim, start > 0, func(Line) error { total++; return nil })
	if err != nil {
		return nil, err
	}
	if start > 0 {
		lg.DroppedLines++
		lg.DroppedBytes += int64(first)
		lg.Skipped += int64(first)
	}
	lg.Format = format
	r := &Result{Format: format, Lines: total}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	// Segunda pasada: ventana a ventana. buf tiene las líneas [base, base+len).
	var (
		buf   []Line
		base  int
		cands []candidate
		next  = 0 // índice global de la siguiente línea
		core  = 0 // principio del núcleo de la ventana en curso
	)
	flush := func() error {
		end := min(core+wo.Lines, total)
		cs, err := r.scoreWindow(ctx, g, &Log{Lines: buf, Format: format}, core-base, end-base, base, total, wo, o)
		if err != nil {
			return err
		}
		for _, c := range cs {
			tail := min(len(buf), c.End+1+wo.Context)
			cands = append(cands, candidate{
				Chunk: Chunk{Start: base + c.Start, End: base + c.End, Score: c.Score, Anchor: base + c.Anchor},
				lines: append([]Line(nil), buf[c.Start:tail]...),
			})
		}
		r.Windows++
		// La ventana siguiente empieza en end; se guarda su contexto de antes.
		keep := max(base, end-wo.Context)
		buf = append(buf[:0], buf[keep-base:]...)
		base, core = keep, end
		return nil
	}
	_, _, err = scanLines(src(), lim, start > 0, func(l Line) error {
		if next >= total {
			return nil // el fichero creció entre las dos pasadas: se ignora lo nuevo
		}
		buf = append(buf, l)
		next++
		if next == min(core+wo.Lines+wo.Context, total) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for core < total && next == total {
		// Las últimas ventanas: su final ya se leyó como contexto de la
		// anterior y no queda ninguna línea que dispare flush.
		if err := flush(); err != nil {
			return nil, err
		}
	}

	// Los mejores tramos de todo el log, con las reglas de Locate.
	tc := time.Now()
	best := pickGlobal(cands, o.Chunk)
	mini := &Log{Format: format}
	local := make([]Chunk, len(best))
	for k, c := range best {
		r.Chunks = append(r.Chunks, ChunkOut{From: c.Start + 1, To: c.End + 1, Score: round3(c.Score)})
		off := len(mini.Lines)
		local[k] = Chunk{Start: off, End: off + c.End - c.Start, Score: c.Score, Anchor: off + c.Anchor - c.Start}
		mini.Lines = append(mini.Lines, c.lines...)
	}
	r.Chunk = ChunkText(mini, local, MaxChunkBytes)
	r.Timing.Locate = ms(time.Since(tc))
	r.setDropped(lg, total)
	if err := decide(ctx, g, r, mini, local, o); err != nil {
		return nil, err
	}
	r.Timing.Total = ms(time.Since(t0))
	return r, nil
}

// scoreWindow puntúa el núcleo [lo, hi) de una ventana w (cuya línea 0 es la
// base-ésima del log) y devuelve los tramos que Locate ancla en él, en índices
// de w. Se puntúan también MaxLines líneas a cada lado para que un tramo
// pueda crecer más allá del borde; un tramo anclado fuera del núcleo es de la
// ventana vecina y se descarta aquí.
func (r *Result) scoreWindow(ctx context.Context, g *Gateway, w *Log, lo, hi, base, total int, wo WindowOptions, o Options) ([]Chunk, error) {
	tf := time.Now()
	slo, shi := max(0, lo-o.Chunk.MaxLines), min(len(w.Lines), hi+o.Chunk.MaxLines)
	in := features(w, slo, shi, base, total)
	r.Timing.Features += ms(time.Since(tf))
	for _, x := range in {
		if x.Index >= lo && x.Index < hi {
			r.Scored++
		}
	}
	tl := time.Now()
	score, chispaMS, err := g.ScoreLines(ctx, o.LinesTask, len(w.Lines), in, o.Workers)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", o.LinesTask, err)
	}
	r.Timing.Lines += ms(time.Since(tl))
	r.Timing.LinesIn += chispaMS
	var out []Chunk
	for _, c := range Locate(w, score, o.Chunk) {
		if c.Anchor >= lo && c.Anchor < hi {
			out = append(out, c)
		}
	}
	return out, nil
}

// pickGlobal elige entre los tramos de todas las ventanas como Locate entre
// anclas: el mejor, y un segundo si puntúa al menos Second × el primero, sin
// solaparse y dentro del tope de bytes. Que cada ventana ya aplicara Second
// no quita nada: lo que no llega a Second × la mejor de su ventana tampoco
// llega a Second × la mejor del log.
func pickGlobal(cands []candidate, o ChunkOptions) []candidate {
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].Score != cands[b].Score {
			return cands[a].Score > cands[b].Score
		}
		return cands[a].Start > cands[b].Start // a igual puntuación, la más tardía
	})
	var out []candidate
	bytes := 0
	for _, c := range cands {
		if len(out) == o.MaxChunks || bytes >= o.MaxBytes {
			break
		}
		if len(out) > 0 && c.Score < o.Second*out[0].Score {
			break
		}
		overlap := false
		for _, p := range out {
			overlap = overlap || c.Start <= p.End && p.Start <= c.End
		}
		if overlap {
			continue
		}
		for _, l := range c.lines[:c.End-c.Start+1] {
			bytes += len(l.Text) + 1
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
