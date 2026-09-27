package tools

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/codificador"
	"github.com/juan52878911/kindling/pkg/von"
)

// La capa 3 desde la CLI: calcular embeddings (una vez, a una caché), entrenar
// la cabeza sobre ellos y cargar codificador + cabeza para decide/eval.
// Diseño y cifras en docs/codificador.md.

// encoderModel busca el codificador del catálogo por su ID o su ref (id:quant).
func encoderModel(name string) (von.Model, error) {
	m, err := von.Find(name, "")
	if err != nil {
		return m, err
	}
	if m.Kind != von.KindEmbed {
		return m, fmt.Errorf("%s is not an encoder (kind %q)", m.Ref(), m.Kind)
	}
	return m, nil
}

func cmdDomoticaEmbed(args []string) error {
	fs := flag.NewFlagSet("domotica embed", flag.ExitOnError)
	data := fs.String("data", "", "comma-separated unified JSONL files whose texts to embed")
	challenge := fs.Bool("challenge", true, "also embed the built-in challenge and indirect sets")
	url := fs.String("url", "", "the encoder: http://host:port of a llama-server --embeddings (a replica's address)")
	model := fs.String("model", "multilingual-e5-small", "catalog encoder the replica serves (sets the prefix)")
	out := fs.String("o", "", "embedding cache (.jemb); an existing one is extended")
	batch := fs.Int("batch", 32, "texts per request")
	conc := fs.Int("concurrency", 2, "requests in flight")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *url == "" || *out == "" {
		return errors.New("usage: kindling-domotica embed -url http://host:port -model <encoder> -data a.jsonl,b.jsonl -o cache.jemb")
	}
	m, err := encoderModel(*model)
	if err != nil {
		return err
	}
	if *batch < 1 || *batch > codificador.MaxBatch || *conc < 1 || *conc > 16 {
		return fmt.Errorf("-batch 1..%d, -concurrency 1..16", codificador.MaxBatch)
	}
	cache := codificador.NewCache(codificador.CacheHeader{Model: m.Ref(), SHA256: m.SHA256, Dim: m.Dim})
	if regularFile(*out) {
		if cache, err = codificador.LoadCache(*out); err != nil {
			return err
		}
		if cache.Header.Model != m.Ref() || cache.Header.SHA256 != m.SHA256 {
			return fmt.Errorf("%s holds vectors of %s, not %s", *out, cache.Header.Model, m.Ref())
		}
	}
	seen := map[string]bool{}
	var todo []string
	add := func(rows []domotica.Row) {
		for _, r := range rows {
			t := m.Prefix + r.Text
			if !seen[t] {
				seen[t] = true
				if _, ok := cache.Get(t); !ok {
					todo = append(todo, t)
				}
			}
		}
	}
	for _, p := range strings.Split(*data, ",") {
		if p == "" {
			continue
		}
		rows, _, err := readRowsFile(p)
		if err != nil {
			return err
		}
		add(rows)
	}
	if *challenge {
		add(domotica.Challenge())
		add(domotica.Indirect(""))
	}
	sort.Strings(todo) // orden fijo: dos pasadas piden lo mismo en el mismo orden
	fmt.Printf("%d distinct texts, %d already in %s, %d to embed with %s\n", len(seen), len(seen)-len(todo), *out, len(todo), m.Ref())

	ctx, stop := ctxWithSignals()
	defer stop()
	emb := &codificador.HTTPEmbedder{URL: *url, Dim: m.Dim}
	work := make(chan []string)
	var mu sync.Mutex
	var firstErr error
	done, t0 := 0, time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				vs, err := emb.Embed(ctx, b)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				for i, v := range vs {
					_ = cache.Put(b[i], v)
				}
				done += len(vs)
				if done%2048 < len(vs) {
					fmt.Printf("  %d/%d (%.0f texts/s)\n", done, len(todo), float64(done)/time.Since(t0).Seconds())
				}
				mu.Unlock()
			}
		}()
	}
send:
	for s := 0; s < len(todo); s += *batch {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed {
			break
		}
		select {
		case work <- todo[s:min(s+*batch, len(todo))]:
		case <-ctx.Done():
			break send
		}
	}
	close(work)
	wg.Wait()
	// Lo calculado se guarda aunque algo fallara: la siguiente pasada sigue.
	if err := cache.Save(*out); err != nil {
		return err
	}
	fmt.Printf("%d vectors in %s (%.1f s)\n", cache.Len(), *out, time.Since(t0).Seconds())
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// encoderDataset convierte filas en vectores de la caché y etiquetas.
func encoderDataset(rows []domotica.Row, cache *codificador.Cache, prefix string, idx map[string]int) (codificador.Dataset, error) {
	var ds codificador.Dataset
	for _, r := range rows {
		v, ok := cache.Get(prefix + r.Text)
		if !ok {
			return ds, fmt.Errorf("%q is not in the embedding cache: run kindling-domotica embed on its file first", r.Text)
		}
		y, ok := idx[r.Intent]
		if !ok {
			return ds, fmt.Errorf("intent %q is not in the training labels", r.Intent)
		}
		ds.X, ds.Y = append(ds.X, v), append(ds.Y, y)
	}
	return ds, nil
}

func cmdDomoticaTrainEncoder(args []string) error {
	fs := flag.NewFlagSet("domotica train-encoder", flag.ExitOnError)
	data := fs.String("data", "", "unified JSONL training data (required)")
	valid := fs.String("valid", "", "unified JSONL validation data (required: early stopping, temperature, thresholds)")
	test := fs.String("test", "", "optional unified JSONL test data (reported, not used)")
	cachePath := fs.String("cache", "", "embedding cache with every text (kindling-domotica embed)")
	out := fs.String("o", "", "output head (.jenc)")
	hidden := fs.Int("hidden", 256, "hidden units (0 = multinomial logistic regression)")
	epochs := fs.Int("epochs", 40, "maximum epochs")
	lr := fs.Float64("lr", 2e-3, "Adam learning rate")
	l2 := fs.Float64("l2", 1e-4, "weight decay")
	// 0,90 y 3: elegidos en validación (docs/codificador.md). Los umbrales se
	// eligen sobre lo que la capa 3 ve de verdad —lo que escalan las rápidas,
	// ~300 órdenes de la habitación en validación—, y con 0,95 y 5 casi
	// ninguna clase llegaba a contestar.
	precision := fs.Float64("precision", 0.90, "target precision of the confident answers of each class")
	minSupport := fs.Int("min-support", 3, "validation predictions a class needs to get a threshold")
	escalated := fs.Bool("escalated", true, "choose thresholds only on the validation rows the fast layers escalate (what layer 3 sees); needs the intent and slot models")
	intentPath := fs.String("intent", "", "intent model (.chispa) for -escalated")
	slotsPath := fs.String("slots", "", "slot model (.chispas) for -escalated")
	knn := fs.Int("knn", 0, "also report a k-nearest-neighbours classifier with this k (comparison only)")
	indirect := fs.Bool("indirect", false, "add the built-in indirect commands (train and valid splits) to the training and validation data (docs/codificador.md: better hints on indirect commands, slightly lower confident precision)")
	seed := fs.Uint64("seed", 1, "random seed")
	verbose := fs.Bool("v", false, "print every epoch")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *data == "" || *valid == "" || *cachePath == "" || *out == "" {
		return errors.New("usage: kindling-domotica train-encoder -data train.jsonl -valid valid.jsonl -cache c.jemb -o head.jenc")
	}
	cache, err := codificador.LoadCache(*cachePath)
	if err != nil {
		return err
	}
	m, err := encoderModel(cache.Header.Model)
	if err != nil {
		return err
	}
	tr, sum, err := readRowsFile(*data)
	if err != nil {
		return err
	}
	va, _, err := readRowsFile(*valid)
	if err != nil {
		return err
	}
	if *indirect {
		tr = append(tr, domotica.Indirect("train")...)
		va = append(va, domotica.Indirect("valid")...)
	}
	var labels []string
	idx := map[string]int{}
	for _, r := range tr {
		if _, ok := idx[r.Intent]; !ok {
			idx[r.Intent] = 0
			labels = append(labels, r.Intent)
		}
	}
	sort.Strings(labels)
	for i, l := range labels {
		idx[l] = i
	}
	dtr, err := encoderDataset(tr, cache, m.Prefix, idx)
	if err != nil {
		return err
	}
	dva, err := encoderDataset(va, cache, m.Prefix, idx)
	if err != nil {
		return err
	}
	created := time.Now().UTC().Format(time.RFC3339)
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			created = time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	}
	cfg := codificador.TrainConfig{Hidden: *hidden, Epochs: *epochs, LR: *lr, L2: *l2, Seed: *seed,
		TargetPrecision: *precision, MinSupport: *minSupport,
		Meta: codificador.Meta{Encoder: m.Ref(), EncoderSHA256: m.SHA256, Prefix: m.Prefix, CreatedAt: created, DatasetSHA256: sum}}
	if *verbose {
		cfg.Log = os.Stderr
	}
	if *escalated {
		d, err := loadDecider(*intentPath, *slotsPath)
		if err != nil {
			return err
		}
		if d.Intent == nil {
			return errors.New("-escalated needs the intent model (-intent, or the default one); or pass -escalated=false")
		}
		cfg.ThresholdMask = make([]bool, len(va))
		n := 0
		for i, r := range va {
			if !d.Decide(r.Text, r.Lang).Confident {
				cfg.ThresholdMask[i] = true
				n++
			}
		}
		fmt.Printf("thresholds on the %d of %d validation rows the fast layers escalate\n", n, len(va))
	}
	t0 := time.Now()
	res, err := codificador.Train(labels, dtr, dva, cfg)
	if err != nil {
		return err
	}
	if err := res.Head.Save(*out); err != nil {
		return err
	}
	st, _ := os.Stat(*out)
	fmt.Printf("trained %s on %d rows in %.1f s: best epoch %d, valid accuracy %.4f, macro-F1 %.4f, int16/float agreement %.4f\n",
		*out, len(tr), time.Since(t0).Seconds(), res.BestEpoch, res.ValidAccuracy, res.ValidMacroF1, res.Agreement)
	fmt.Printf("encoder %s, prefix %q, %d labels, hidden %d, temperature %.3f, %d bytes\n",
		m.Ref(), m.Prefix, len(labels), *hidden, res.Head.Temperature, st.Size())
	never := 0
	for _, t := range res.Head.Thresholds {
		if t > 1 {
			never++
		}
	}
	fmt.Printf("%d of %d classes never answer confidently (too few validation rows at %.2f precision)\n", never, len(labels), *precision)
	if *test != "" || *knn > 0 {
		sets := map[string]codificador.Dataset{"valid": dva}
		names := []string{"valid"}
		if *test != "" {
			te, _, err := readRowsFile(*test)
			if err != nil {
				return err
			}
			if sets["test"], err = encoderDataset(te, cache, m.Prefix, idx); err != nil {
				return err
			}
			names = append(names, "test")
		}
		var kn *codificador.KNN
		if *knn > 0 {
			kn = codificador.NewKNN(*knn, labels, dtr)
		}
		for _, n := range names {
			ds := sets[n]
			ok, kok := 0, 0
			for i, v := range ds.X {
				p, err := res.Head.Predict(v)
				if err != nil {
					return err
				}
				if p.Index == ds.Y[i] {
					ok++
				}
				if kn != nil {
					if k, _ := kn.Predict(v); k == ds.Y[i] {
						kok++
					}
				}
			}
			fmt.Printf("%-5s head accuracy %.4f", n, float64(ok)/float64(len(ds.X)))
			if kn != nil {
				fmt.Printf("   %d-NN accuracy %.4f", *knn, float64(kok)/float64(len(ds.X)))
			}
			fmt.Println()
		}
	}
	return nil
}

// loadEncoder arma la capa 3: la cabeza y su codificador, que es una caché de
// vectores (-embed-cache, reproducible), una réplica (-embed-url) o las dos
// (la caché primero).
func loadEncoder(headPath, cachePath, url string) (*codificador.Layer, error) {
	if headPath == "" {
		return nil, nil
	}
	h, err := codificador.LoadFile(headPath)
	if err != nil {
		return nil, err
	}
	var next codificador.Embedder
	if url != "" {
		next = &codificador.HTTPEmbedder{URL: url, Dim: h.Dim}
	}
	if cachePath == "" {
		if next == nil {
			return nil, errors.New("the encoder head needs its encoder: -embed-url http://host:port and/or -embed-cache file.jemb")
		}
		return &codificador.Layer{Head: h, Embedder: next}, nil
	}
	c, err := codificador.LoadCache(cachePath)
	if err != nil {
		return nil, err
	}
	if c.Header.Model != h.Meta.Encoder || (h.Meta.EncoderSHA256 != "" && c.Header.SHA256 != h.Meta.EncoderSHA256) {
		return nil, fmt.Errorf("%s holds vectors of %s, but the head was trained on %s", cachePath, c.Header.Model, h.Meta.Encoder)
	}
	return &codificador.Layer{Head: h, Embedder: &codificador.Cached{Cache: c, Next: next}}, nil
}

// encoderSystems son los sistemas que añade la capa 3 a `domotica eval`.
func encoderSystems(d *domotica.Decider, enc *codificador.Layer) []namedSystem {
	if enc == nil {
		return nil
	}
	alone, casc := *d, *d
	alone.Encoder, alone.OnlyEncoder, alone.FinalOOS = enc, true, true
	casc.Encoder = enc
	ctx := context.Background()
	return []namedSystem{
		{"encoder", func(t, l string) domotica.Decision { return alone.DecideContext(ctx, t, l) }},
		{"cascade + encoder", func(t, l string) domotica.Decision { return casc.DecideContext(ctx, t, l) }},
	}
}

type namedSystem = struct {
	name string
	sys  domotica.System
}
