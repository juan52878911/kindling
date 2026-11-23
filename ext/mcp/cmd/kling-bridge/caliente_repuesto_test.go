package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
)

// LO QUE PAGAN LAS SESIONES QUE LLEGAN DESPUÉS DE LA PRIMERA.
//
// Tras restaurar el dorado hay UN hijo caliente: la primera sesión lo adopta y
// no paga el arranque del runtime. Las siguientes de la misma instancia sí.

const arranqueFalsoMs = 300

// sinMMDS hace que el almacén MMDS conteste vacío, sin salir a la red.
func sinMMDS(tb testing.TB) {
	antes := fetchMMDS
	fetchMMDS = func() (*guest.MMDSStore, error) { return nil, nil }
	tb.Cleanup(func() { fetchMMDS = antes })
}

// initializeSesion abre una sesión y mide hasta la respuesta del initialize.
func initializeSesion(tb testing.TB, b *bridge) (time.Duration, *session) {
	tb.Helper()
	t0 := time.Now()
	s, _, err := b.resolve("", true)
	if err != nil {
		tb.Fatalf("resolve: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.request(ctx, `"i"`, []byte(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{}}`)); err != nil {
		tb.Fatalf("initialize: %v", err)
	}
	b.release(s)
	return time.Since(t0), s
}

// BenchmarkSesionesTrasRestaurar: un puente con su caliente (como tras
// restaurar) recibe tres sesiones separadas por más de lo que tarda el
// runtime en arrancar; ms_sesionN es lo que tardó el initialize de la N.
func BenchmarkSesionesTrasRestaurar(b *testing.B) {
	b.Setenv(envArranqueFalso, fmt.Sprint(arranqueFalsoMs))
	sinMMDS(b)
	var suma [3]time.Duration
	for range b.N {
		b.StopTimer()
		br := testBridge()
		if err := br.replenishWarm(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for i := range 3 {
			d, _ := initializeSesion(b, br)
			suma[i] += d
			b.StopTimer()
			time.Sleep(2 * arranqueFalsoMs * time.Millisecond)
			b.StartTimer()
		}
		b.StopTimer()
		br.closeAll()
		b.StartTimer()
	}
	for i, d := range suma {
		b.ReportMetric(float64(d.Milliseconds())/float64(b.N), fmt.Sprintf("ms_sesion%d", i+1))
	}
}

// esperarCaliente espera a que reponerCaliente deje un caliente en el puente.
func esperarCaliente(t *testing.T, b *bridge) *warmChild {
	t.Helper()
	for range 250 {
		b.mu.Lock()
		w := b.warm
		b.mu.Unlock()
		if w != nil {
			return w
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no se repuso el hijo caliente tras gastar el que había")
	return nil
}

// Tras adoptar el caliente se repone otro, y la sesión siguiente lo adopta en
// vez de lanzar un proceso.
func TestSeReponeElCalienteTrasAdoptarlo(t *testing.T) {
	sinMMDS(t)
	b := testBridge()
	t.Cleanup(b.closeAll)
	if err := b.replenishWarm(); err != nil {
		t.Fatal(err)
	}
	_, s1 := initializeSesion(t, b)
	w := esperarCaliente(t, b)
	_, s2 := initializeSesion(t, b)
	if s2.cmd.Process.Pid != w.cmd.Process.Pid {
		t.Fatalf("la segunda sesión lanzó un proceso (pid %d) en vez de adoptar el repuesto (pid %d)",
			s2.cmd.Process.Pid, w.cmd.Process.Pid)
	}
	if s1.cmd.Process.Pid == s2.cmd.Process.Pid {
		t.Fatal("dos sesiones comparten proceso")
	}
}

// Sin plaza para otra sesión no se repone: el caliente ocuparía la memoria de
// una sesión que no cabe.
func TestNoSeReponeSinPlaza(t *testing.T) {
	sinMMDS(t)
	b := testBridge()
	b.maxSessions = 1
	t.Cleanup(b.closeAll)
	if err := b.replenishWarm(); err != nil {
		t.Fatal(err)
	}
	_, _ = initializeSesion(t, b)
	time.Sleep(3 * arranqueFalsoMs * time.Millisecond)
	b.mu.Lock()
	w, rep := b.warm, b.reponiendo
	b.mu.Unlock()
	if w != nil || rep != nil {
		t.Fatalf("con tope 1 y la plaza ocupada se repuso un caliente (warm=%v, reponiendo=%v)", w != nil, rep != nil)
	}
}

// Con secretos en MMDS el caliente del dorado no vale (no los lleva), pero el
// repuesto se lanza con ellos y la sesión siguiente sí lo adopta. Y un /reset
// no deja ese caliente con secretos para el dorado: lo cambia por uno con el
// entorno base.
func TestCalienteRepuestoConSecretosYReset(t *testing.T) {
	store := &guest.MMDSStore{Env: map[string]string{"SECRETO_FALSO": "s3"}}
	conMMDS(t, &store)
	b := testBridge()
	// Con entorno base explícito, como en la microVM: si no, el entorno de
	// MMDS sería SOLO el secreto y el hijo no sabría que es el servidor falso.
	b.env = os.Environ()
	t.Cleanup(b.closeAll)
	if err := b.replenishWarm(); err != nil {
		t.Fatal(err)
	}
	dorado := b.warm
	_, s1 := initializeSesion(t, b)
	if s1.cmd.Process.Pid == dorado.cmd.Process.Pid {
		t.Fatal("la sesión con secretos adoptó el caliente lanzado sin ellos")
	}
	w := esperarCaliente(t, b)
	env, _ := b.sessionEnv("abcdef0123456789")
	if w.huella != huellaEnv(env) {
		t.Fatal("el repuesto no lleva el entorno de MMDS")
	}
	_, s2 := initializeSesion(t, b)
	if s2.cmd.Process.Pid != w.cmd.Process.Pid {
		t.Fatalf("la sesión con secretos no adoptó el repuesto que los lleva (pid %d, repuesto %d)",
			s2.cmd.Process.Pid, w.cmd.Process.Pid)
	}

	// Otro repuesto con secretos, y un /reset: el dorado no debe llevárselo.
	conSecretos := esperarCaliente(t, b)
	b.handleReset(httptest.NewRecorder(), httptest.NewRequest("POST", "/reset", nil))
	b.mu.Lock()
	tras := b.warm
	b.mu.Unlock()
	if tras == nil || tras == conSecretos || tras.huella != huellaEnv(b.env) {
		t.Fatal("tras /reset el caliente no es uno nuevo con el entorno base")
	}
}
