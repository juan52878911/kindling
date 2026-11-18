package machine

import (
	"os"
	"strings"
	"testing"
)

// F4 (ronda 5): un volumen guarda sus etiquetas (compose pone su proyecto),
// Volumes las devuelve, y al borrarlo se van con él: un volumen nuevo con el
// mismo nombre no hereda las del anterior.
func TestEtiquetasDeVolumen(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")
	escribirVolumen(t, m, "suelto", "v1")
	if err := m.escribirEtiquetasVolumen("datos", map[string]string{"kling.compose": "a"}); err != nil {
		t.Fatal(err)
	}
	por := map[string]map[string]string{}
	for _, v := range m.Volumes() {
		por[v.Name] = v.Labels
	}
	if len(por) != 2 || por["datos"]["kling.compose"] != "a" || por["suelto"] != nil {
		t.Fatalf("etiquetas = %v", por)
	}
	if err := m.RemoveVolume("datos", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.volumeLabelsPath("datos")); !os.IsNotExist(err) {
		t.Fatalf("las etiquetas siguen: %v", err)
	}
	escribirVolumen(t, m, "datos", "v2")
	if v, err := m.statVolume("datos"); err != nil || v.Labels != nil {
		t.Fatalf("el nuevo heredó %v (%v)", v, err)
	}
	// Sin etiquetas se quita una que quedara suelta.
	if err := os.WriteFile(m.volumeLabelsPath("datos"), []byte(`{"kling.compose":"viejo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.escribirEtiquetasVolumen("datos", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.statVolume("datos"); v == nil || v.Labels != nil {
		t.Fatalf("quedó %v", v)
	}
}

func TestEtiquetasDeVolumenAcotadas(t *testing.T) {
	m := newTestManager(t)
	muchas := map[string]string{}
	for i := range maxEtiquetasVolumen + 1 {
		muchas[strings.Repeat("k", i+1)] = "v"
	}
	for _, l := range []map[string]string{muchas, {"": "v"}, {"k": strings.Repeat("v", maxValorEtiqueta+1)}} {
		if _, err := m.CreateVolumeLabeled(t.Context(), "notas", 64, l); err == nil || !strings.Contains(err.Error(), "label") {
			t.Fatalf("aceptó %d etiquetas", len(l))
		}
	}
	if _, err := os.Stat(m.volumePath("notas")); !os.IsNotExist(err) {
		t.Fatalf("creó el volumen: %v", err)
	}
}
