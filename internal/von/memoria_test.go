package von

import "testing"

// La memoria por defecto de un modelo deja sitio a lo que llama-server pide
// de verdad. Medido en EC2 (x86, ronda 9): Qwen2.5-1.5B Q8_0 (GGUF de 1807
// MiB) moría por OOM una y otra vez en 2368 MiB (2310 visibles) con 2,34 GB de
// anon-rss y 2,8 GB de total-vm a mitad de carga; el Q4_K_M (1066 MiB) cargaba
// en 1600 con un dorado de 1245 MiB. La de Q8_0 llega a 2,9 GiB con la caché
// de prompts; las de 4 bits no suben.
func TestMemoriaPorDefectoDejaMargen(t *testing.T) {
	q8, err := Find("qwen2.5-1.5b-instruct", "q8_0")
	if err != nil {
		t.Fatal(err)
	}
	if got := q8.MemDefault() + DefaultCacheRAM; got < 2970 {
		t.Errorf("Q8_0 de 1,5B: %d MiB con la caché de prompts; hacen falta al menos ~2,9 GiB", got)
	}
	q4, err := Find("qwen2.5-1.5b-instruct", "q4_k_m")
	if err != nil {
		t.Fatal(err)
	}
	if got := q4.MemDefault(); got != 1536 {
		t.Errorf("Q4_K_M de 1,5B: %d MiB; con 1536 (+64 de caché) ya cargaba", got)
	}
	for _, m := range Catalog {
		if m.Kind == KindEmbed {
			continue
		}
		pesos := int(m.Size >> 20)
		d := m.MemDefault()
		if d < m.MemMiB {
			t.Errorf("%s: %d MiB, menos que lo medido (%d)", m.Ref(), d, m.MemMiB)
		}
		if d%64 != 0 {
			t.Errorf("%s: %d MiB no es múltiplo de 64", m.Ref(), d)
		}
		if d < pesos+memBase {
			t.Errorf("%s: %d MiB no cubre los pesos (%d) y la base (%d)", m.Ref(), d, pesos, memBase)
		}
		if m.Quant != "q8_0" && d != m.MemMiB {
			t.Errorf("%s: la fórmula sube un modelo de 4 bits de %d a %d MiB", m.Ref(), m.MemMiB, d)
		}
		if m.Quant == "q8_0" && d < pesos+pesos/2+memBase {
			t.Errorf("%s: %d MiB no cubre el transitorio de carga de Q8_0", m.Ref(), d)
		}
	}
	// Los codificadores no pasan por la fórmula: no tienen caché KV de
	// generación ni ese transitorio medido.
	for _, m := range embedCatalog {
		if d := m.MemDefault(); d != m.MemMiB {
			t.Errorf("%s: %d MiB, quería lo medido (%d)", m.Ref(), d, m.MemMiB)
		}
	}
}
