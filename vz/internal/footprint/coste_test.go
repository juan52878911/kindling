package footprint

import (
	"math"
	"testing"
	"time"
)

// Las cifras del M4 (coste.go): el mismo bucle, 6.372 M instrucciones en 328 ms
// de P sin techo y 6.307 M en 1.140 ms de E con techo 25 %. Un ms en E vale
// ~0,29 ms en P.
func TestRelativaEConLasCifrasDelM4(t *testing.T) {
	l := lectura{
		total: 328*time.Millisecond + 1140*time.Millisecond, p: 328 * time.Millisecond,
		instr: 6372e6 + 6307e6, instrP: 6372e6,
	}
	if k := relativaE(l); math.Abs(k-0.288) > 0.01 {
		t.Fatalf("relativaE = %.3f, quería ~0,288", k)
	}
}

// Sin muestra suficiente en P o en E, o en un Mac sin núcleos P/E
// (ri_user_ptime a 0), se cobra el tiempo tal cual. Y la proporción está
// acotada: ni más que P ni menos que relMin.
func TestRelativaELimites(t *testing.T) {
	for _, tc := range []struct {
		nombre string
		l      lectura
		want   float64
	}{
		{"sin P/E (Intel)", lectura{total: time.Second, instr: 1e9}, 1},
		{"poco en P", lectura{total: time.Second, p: 5 * time.Millisecond, instr: 2e9, instrP: 1e8}, 1},
		{"poco en E", lectura{total: time.Second, p: time.Second - time.Millisecond, instr: 2e9, instrP: 2e9 - 1}, 1},
		{"todo en P", lectura{total: time.Second, p: time.Second, instr: 4e9, instrP: 4e9}, 1},
		{"E más rápido que P", lectura{total: 2 * time.Second, p: time.Second, instr: 5e9, instrP: 1e9}, 1},
		{"E absurdo", lectura{total: 2 * time.Second, p: time.Second, instr: 4e9 + 1e6, instrP: 4e9}, relMin},
	} {
		if got := relativaE(tc.l); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: relativaE = %v, quería %v", tc.nombre, got, tc.want)
		}
	}
}

// El contador cobra el tiempo en P entero y el de E a su proporción, no baja
// nunca aunque la proporción cambie, y se reinicia con un auxiliar nuevo.
func TestContadorEquivalente(t *testing.T) {
	var c contador
	// Arranque: 400 ms en P a 20 instrucciones/ns.
	l := lectura{total: 400 * time.Millisecond, p: 400 * time.Millisecond, instr: 8e9, instrP: 8e9}
	if got := c.suma(l); got != 400*time.Millisecond {
		t.Fatalf("primera lectura = %v", got)
	}
	// 100 ms más en E a 5 instrucciones/ns: valen 25 ms de P.
	l.total += 100 * time.Millisecond
	l.instr += 5e8
	got := c.suma(l)
	if d := got - 400*time.Millisecond; d < 24*time.Millisecond || d > 26*time.Millisecond {
		t.Fatalf("100 ms en E cobrados como %v, quería ~25 ms", d)
	}
	// 10 ms en P: enteros.
	prev := got
	l.total += 10 * time.Millisecond
	l.p += 10 * time.Millisecond
	l.instr += 2e8
	l.instrP += 2e8
	if d := c.suma(l) - prev; d < 10*time.Millisecond || d > 11*time.Millisecond {
		t.Fatalf("10 ms en P cobrados como %v", d)
	}
	// Un E mucho más lento cambia la proporción, pero lo ya cobrado no baja.
	prev = c.equiv
	l.total += 50 * time.Millisecond
	l.instr += 1e6
	if got := c.suma(l); got < prev {
		t.Fatalf("el contador bajó: %v -> %v", prev, got)
	}
	// Un auxiliar nuevo (contadores más bajos): se parte de su total.
	if got := c.suma(lectura{total: 30 * time.Millisecond}); got != 30*time.Millisecond {
		t.Fatalf("auxiliar nuevo = %v", got)
	}
}

// Lo que importa al techo: una VM que corre en E a ~0,29 de P, bajo un techo
// del 25 %, recibe en E el tiempo que hace ~25 % del trabajo de un núcleo P
// (antes, el 25 % del tiempo, en E: ~7 %, de ahí el ×14).
func TestTechoEnNucleosE(t *testing.T) {
	const pct = 25
	var c contador
	l := lectura{total: 400 * time.Millisecond, p: 400 * time.Millisecond, instr: 8e9, instrP: 8e9}
	base := c.suma(l)
	paso := time.Millisecond
	var pared, enE time.Duration
	var cobrado time.Duration
	// Regulador ideal: corre en E mientras lo cobrado no pase de pct % de la
	// pared.
	for pared = 0; pared < 10*time.Second; pared += paso {
		if cobrado*100 <= pared*pct {
			enE += paso
			l.total += paso
			l.instr += uint64(float64(paso) * 20 * 0.29) // 0,29 de la velocidad de P
		}
		cobrado = c.suma(l) - base
	}
	trabajo := float64(enE) * 0.29 / float64(pared) // en núcleos P
	if trabajo < 0.23 || trabajo > 0.27 {
		t.Fatalf("trabajo hecho = %.3f de un núcleo P, quería ~0,25", trabajo)
	}
}
