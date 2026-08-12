package main

import "testing"

// El tope de sesiones sale de la memoria del invitado, porque cada sesión es un
// PROCESO más del servidor MCP dentro de la microVM.
//
// El 32 fijo de antes hacía que una microVM de 1 GiB aceptara 32 servidores de
// node: el invitado se quedaba sin memoria a la mitad, y el síntoma era el peor
// posible — las sesiones ya abiertas morían por señal mientras el puente seguía
// vivo aceptando más.
func TestElTopeDeSesionesSaleDeLaMemoria(t *testing.T) {
	casos := []struct {
		memMiB int
		want   int
	}{
		{1024, 13}, // (1024-192)/64
		{2048, 29}, // (2048-192)/64
		{4096, 32}, // topado: cada sesión es también un proceso y un descriptor
		{512, 5},
		{256, 1}, // (256-192)/64 = 1
		{192, 1}, // nada repartible: al menos una, o la microVM es inútil
		{64, 1},  // idem
		{0, 1},   // idem
	}
	for _, c := range casos {
		if got := deriveMaxSessions(c.memMiB); got != c.want {
			t.Errorf("con %d MiB: %d sesiones, quería %d", c.memMiB, got, c.want)
		}
	}
}

// Nunca cero: un tope de cero deja la microVM inservible sin decir por qué. Es
// mejor aceptar una sesión y que falle por memoria, con su mensaje, que
// rechazarlas todas desde el arranque.
func TestElTopeNuncaEsCero(t *testing.T) {
	for mem := 0; mem <= 512; mem += 16 {
		if n := deriveMaxSessions(mem); n < 1 {
			t.Fatalf("con %d MiB devolvió %d", mem, n)
		}
	}
}
