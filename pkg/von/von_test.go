package von

import (
	"regexp"
	"strings"
	"testing"
)

func TestCatalogoFijado(t *testing.T) {
	// Cada entrada tiene que estar fijada de verdad: sin revisión o sin hash, el
	// dorado dejaría de ser reproducible sin que nada lo avisara.
	hex40 := regexp.MustCompile(`^[0-9a-f]{40}$`)
	visto := map[string]bool{}
	for _, m := range Catalog {
		if visto[m.Ref()] {
			t.Errorf("%s repetido", m.Ref())
		}
		visto[m.Ref()] = true
		if m.Source != "" {
			continue // convertido por kindling: lo comprueba TestCatalogoEmbed
		}
		if !hex40.MatchString(m.Revision) || !reSHA256.MatchString(m.SHA256) {
			t.Errorf("%s sin fijar: revisión %q, sha256 %q", m.Ref(), m.Revision, m.SHA256)
		}
		if m.Size <= 0 || m.MemMiB <= int(m.Size>>20) || m.VCPUs < 1 {
			t.Errorf("%s: tamaño %d, mem %d MiB, vcpus %d", m.Ref(), m.Size, m.MemMiB, m.VCPUs)
		}
		// La URL del catálogo tiene que pasar la misma validación que una propia.
		if _, err := (Spec{URL: m.URL(), SHA256: m.SHA256}).Resolve(); err != nil {
			t.Errorf("%s: su URL no valida: %v", m.Ref(), err)
		}
	}
	for arch, a := range LlamaAssets {
		if !reSHA256.MatchString(a.SHA256) || !strings.Contains(a.File, LlamaTag) {
			t.Errorf("llama.cpp %s mal fijado: %+v", arch, a)
		}
	}
}

func TestFind(t *testing.T) {
	m, err := Find("smollm2-360m-instruct", "")
	if err != nil || m.Quant != "q8_0" {
		t.Fatalf("por defecto q8_0: %+v %v", m, err)
	}
	if m, err := Find("Qwen2.5-0.5B-Instruct:Q4_K_M", ""); err != nil || m.Quant != "q4_k_m" {
		t.Fatalf("id:quant: %+v %v", m, err)
	}
	if _, err := Find("qwen2.5-0.5b-instruct:q8_0", "q4_k_m"); err == nil {
		t.Fatal("id:quant y -quant distintos deberían fallar")
	}
	if _, err := Find("smollm2-360m-instruct", "q2_k"); err == nil || !strings.Contains(err.Error(), "q4_k_m, q8_0") {
		t.Fatalf("una cuantización que no hay debería listar las que sí: %v", err)
	}
	// Los modelos fuera del catálogo por defecto (licencia no abierta) no
	// salen en IDs y solo se resuelven aceptando SU licencia.
	for _, id := range IDs() {
		if id == "qwen2.5-3b-instruct" {
			t.Fatal("a qwen-research model listed in the default catalog")
		}
	}
	if _, err := (Spec{Model: "qwen2.5-3b-instruct"}).Resolve(); err == nil || !strings.Contains(err.Error(), "-accept-license qwen-research") {
		t.Fatalf("3b without accepting its license: %v", err)
	}
	if _, err := (Spec{Model: "qwen2.5-3b-instruct", AcceptLicense: "apache-2.0"}).Resolve(); err == nil {
		t.Fatal("accepting another license must not do")
	}
	if _, err := (Spec{Model: "qwen2.5-3b-instruct", AcceptLicense: "qwen-research"}).Resolve(); err != nil {
		t.Fatalf("3b accepting its license: %v", err)
	}
	for _, m := range Catalog {
		if m.Source != "" {
			continue
		}
		if m.License == "" || !strings.HasPrefix(m.LicenseURL, "https://huggingface.co/"+m.Repo+"/blob/"+m.Revision+"/") {
			t.Errorf("%s: license %q at %q", m.Ref(), m.License, m.LicenseURL)
		}
	}
	// Sin Q8_0 en el catálogo, sin -quant vale la que hay.
	if m, err := Find("qwen2.5-3b-instruct", ""); err != nil || m.Quant != "q4_k_m" {
		t.Fatalf("3b sin -quant: %+v %v", m, err)
	}
	if _, err := Find("qwen2.5-3b-instruct", "q8_0"); err == nil {
		t.Fatal("pedir q8_0 del 3b a propósito debería fallar")
	}
	if _, err := Find("llama-70b", ""); err == nil {
		t.Fatal("un modelo desconocido debería fallar")
	}
}

func TestResolve(t *testing.T) {
	r, err := Spec{Model: "smollm2-360m-instruct"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r.Ctx != DefaultCtx || r.Parallel != 1 || r.Threads != 0 || r.Ref != "smollm2-360m-instruct:q8_0" {
		t.Fatalf("valores por defecto: %+v", r)
	}
	if r.ModelPath() != "/models/smollm2-360m-instruct-q8_0.gguf" {
		t.Fatalf("ruta: %s", r.ModelPath())
	}

	url := "https://huggingface.co/org/repo-GGUF/resolve/0123456789abcdef0123456789abcdef01234567/Model-Q8_0.gguf"
	sum := strings.Repeat("a", 64)
	r, err = Spec{URL: url, SHA256: sum, Ctx: 4096, Parallel: 2}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r.File != "Model-Q8_0.gguf" || r.Ref != "model-q8_0" || r.Model != nil {
		t.Fatalf("gguf propio: %+v", r)
	}

	malos := []Spec{
		{},
		{Model: "smollm2-360m-instruct", URL: url, SHA256: sum},
		{Model: "smollm2-360m-instruct", SHA256: sum},
		{URL: url},                // sin hash
		{URL: url, SHA256: "ABC"}, // hash mal formado
		{URL: url, SHA256: sum, Quant: "q8_0"},
		{URL: strings.Replace(url, "0123456789abcdef0123456789abcdef01234567", "main", 1), SHA256: sum}, // rama, no commit
		{URL: strings.Replace(url, "https://huggingface.co", "https://evil.example", 1), SHA256: sum},
		{URL: strings.Replace(url, "Model-Q8_0.gguf", "sub/Model.gguf", 1), SHA256: sum},
		{URL: strings.Replace(url, "Model-Q8_0.gguf", "x';reboot;'.gguf", 1), SHA256: sum},
		{Model: "smollm2-360m-instruct", Ctx: 100},
		{Model: "smollm2-360m-instruct", Parallel: 17},
		{Model: "smollm2-360m-instruct", Ctx: 512, Parallel: 4},
		{Model: "smollm2-360m-instruct", Threads: -1},
	}
	for _, s := range malos {
		if _, err := s.Resolve(); err == nil {
			t.Errorf("debería fallar: %+v", s)
		}
	}
}

func TestRunScript(t *testing.T) {
	r, err := Spec{Model: "qwen2.5-0.5b-instruct", Quant: "q4_k_m"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	s := r.RunScript()
	for _, want := range []string{
		"#!/bin/sh\n",
		"THREADS=$(nproc)\n",
		`exec /opt/llama.cpp/llama-server '--model' '/models/qwen2.5-0.5b-instruct-q4_k_m.gguf'`,
		`'--port' '8000'`, `'--ctx-size' '2048'`, `'--threads' "$THREADS"`, `'--cache-ram' '0'`,
		`'--alias' 'qwen2.5-0.5b-instruct:q4_k_m'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("falta %q en:\n%s", want, s)
		}
	}
	r.Threads = 3
	if s := r.RunScript(); !strings.Contains(s, "THREADS=3\n") || strings.Contains(s, "nproc") {
		t.Errorf("hilos fijos:\n%s", s)
	}
}

func TestLabels(t *testing.T) {
	l := Labels("m:q8_0", "von-smol", map[string]string{"kling.ports": "9999", "x": "y"})
	if l["kling.ports"] != "8000" || l[LabelModel] != "m:q8_0" || l["service"] != "von-smol" || l["x"] != "y" {
		t.Fatalf("etiquetas: %v", l)
	}
}
