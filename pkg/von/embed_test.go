package von

import (
	"regexp"
	"strings"
	"testing"
)

func TestCatalogoEmbed(t *testing.T) {
	hex40 := regexp.MustCompile(`^[0-9a-f]{40}$`)
	n := 0
	for _, m := range Catalog {
		if m.Kind == "" {
			if m.Pooling != "" || m.Prefix != "" || m.Source != "" {
				t.Errorf("%s: campos de codificador en un modelo instruct", m.Ref())
			}
			continue
		}
		n++
		// Convertidos por kindling: fijados por los pesos de origen y por el
		// hash de la salida de scripts/encoder-gguf.sh.
		if m.Kind != KindEmbed || !poolings[m.Pooling] || m.Dim <= 0 || m.Repo != "" || m.URL() != "" {
			t.Errorf("%s: kind %q pooling %q dim %d repo %q", m.Ref(), m.Kind, m.Pooling, m.Dim, m.Repo)
		}
		if !hex40.MatchString(m.SourceRevision) || !reSHA256.MatchString(m.SHA256) || m.Size <= 0 {
			t.Errorf("%s sin fijar: %+v", m.Ref(), m)
		}
		if !m.Open() || !strings.HasPrefix(m.LicenseURL, "https://huggingface.co/"+m.Source+"/blob/"+m.SourceRevision+"/") {
			t.Errorf("%s: licencia %q en %q", m.Ref(), m.License, m.LicenseURL)
		}
		if m.MemMiB <= int(m.Size>>20) || m.VCPUs < 1 {
			t.Errorf("%s: mem %d MiB, vcpus %d", m.Ref(), m.MemMiB, m.VCPUs)
		}
	}
	if n < 2 {
		t.Fatalf("faltan codificadores en el catálogo: %d", n)
	}
}

func TestResolveEmbed(t *testing.T) {
	r, err := Spec{Model: "multilingual-e5-small"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindEmbed || r.Pooling != "mean" || r.Ctx != DefaultEmbedCtx || r.URL != "" || r.Model.Prefix != "query: " {
		t.Fatalf("e5: %+v", r)
	}
	s := r.RunScript()
	for _, want := range []string{`'--embeddings'`, `'--pooling' 'mean'`, `'--ubatch-size' '512'`, `'--batch-size' '512'`, `'--ctx-size' '512'`} {
		if !strings.Contains(s, want) {
			t.Errorf("falta %q en:\n%s", want, s)
		}
	}
	// Un instruct no lleva nada de esto.
	c, _ := Spec{Model: "smollm2-360m-instruct"}.Resolve()
	if c.Kind != "" || len(c.EmbedArgs()) != 0 || strings.Contains(c.RunScript(), "--embeddings") {
		t.Fatalf("instruct con argumentos de codificador: %+v", c)
	}
	// Un GGUF propio puede ser codificador si lo dice.
	url := "https://huggingface.co/org/repo/resolve/0123456789abcdef0123456789abcdef01234567/enc.gguf"
	sum := strings.Repeat("b", 64)
	p, err := Spec{URL: url, SHA256: sum, Kind: KindEmbed, Pooling: "cls", Parallel: 2}.Resolve()
	if err != nil || p.Pooling != "cls" || p.Ctx != DefaultEmbedCtx {
		t.Fatalf("propio: %+v %v", p, err)
	}
	if a := strings.Join(p.EmbedArgs(), " "); !strings.Contains(a, "--ubatch-size 256") {
		t.Errorf("con 2 ranuras el lote es el contexto de una: %s", a)
	}
	malos := []Spec{
		{Model: "multilingual-e5-small", Pooling: "cls"},         // el catálogo manda
		{Model: "smollm2-360m-instruct", Kind: KindEmbed},        // no es un codificador
		{URL: url, SHA256: sum, Pooling: "mean"},                 // pooling sin kind
		{URL: url, SHA256: sum, Kind: "rerank"},                  // kind desconocido
		{URL: url, SHA256: sum, Kind: KindEmbed, Pooling: "max"}, // pooling desconocido
	}
	for _, s := range malos {
		if _, err := s.Resolve(); err == nil {
			t.Errorf("debería fallar: %+v", s)
		}
	}
}
