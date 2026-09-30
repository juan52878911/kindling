package lazyre

import (
	"sync"
	"testing"
)

func TestPerezosa(t *testing.T) {
	r := New(`^a+(b)$`)
	if !r.MatchString("aab") || r.MatchString("b") {
		t.Fatal("MatchString")
	}
	if m := r.FindStringSubmatch("ab"); len(m) != 2 || m[1] != "b" {
		t.Fatalf("FindStringSubmatch = %q", m)
	}
	if got := r.ReplaceAllString("ab", "x$1"); got != "xb" {
		t.Fatalf("ReplaceAllString = %q", got)
	}
	if r.String() != `^a+(b)$` || r.Regexp() != r.Regexp() {
		t.Fatal("String/Regexp")
	}
}

func TestConcurrente(t *testing.T) {
	r := New(`^[0-9]+$`)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !r.MatchString("123") {
				t.Error("MatchString")
			}
		}()
	}
	wg.Wait()
}

func TestMalaEnPrimerUso(t *testing.T) {
	r := New(`(`)
	if CompileAll() == nil {
		t.Fatal("CompileAll no vio la expresión mala")
	}
	todas = todas[:len(todas)-1]
	defer func() {
		if recover() == nil {
			t.Fatal("sin pánico en el primer uso")
		}
	}()
	r.MatchString("x")
}
