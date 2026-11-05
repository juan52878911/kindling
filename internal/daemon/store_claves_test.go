package daemon

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// Escrituras a la vez a claves distintas y a la misma: cada clave acaba con
// uno de los valores escritos, entero, y no quedan cerrojos de clave.
func TestStorePutAlaVez(t *testing.T) {
	st := &store{dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := range 16 {
		for j := range 4 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if err := st.put("ns", "k"+strconv.Itoa(i), []byte(`{"v":`+strconv.Itoa(j)+`}`)); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				defer wg.Done()
				if err := st.put("ns", "comun", []byte(`{"de":`+strconv.Itoa(i)+`}`)); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	ks, err := st.keys("ns")
	if err != nil || len(ks) != 17 {
		t.Fatalf("keys = %v, %v", ks, err)
	}
	for _, k := range ks {
		b, err := st.get("ns", k)
		if err != nil || len(b) < 7 || b[0] != '{' || b[len(b)-1] != '}' {
			t.Fatalf("%s = %q, %v", k, b, err)
		}
	}
	if err := st.delete("ns", "comun"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n := len(st.claves)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d cerrojos de clave sin soltar", n)
	}
}

// Una escritura lenta de una clave no frena la de otra.
func TestStoreClavesNoSeEsperan(t *testing.T) {
	st := &store{dir: t.TempDir()}
	p1, _ := st.path("ns", "lenta")
	soltar := st.cerrar(p1) // como si un put de "lenta" estuviera a medias
	hecho := make(chan error, 1)
	go func() { hecho <- st.put("ns", "otra", []byte(`{}`)) }()
	select {
	case err := <-hecho:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("el put de otra clave esperó al de lenta")
	}
	espera := make(chan error, 1)
	go func() { espera <- st.put("ns", "lenta", []byte(`{}`)) }()
	select {
	case <-espera:
		t.Fatal("dos escrituras de la misma clave a la vez")
	case <-time.After(50 * time.Millisecond):
	}
	soltar()
	if err := <-espera; err != nil {
		t.Fatal(err)
	}
}
