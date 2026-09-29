package main

import (
	"reflect"
	"testing"
)

// La clave de producción de kling db clone no llega al entorno de db-golden.sh.
func TestSinClavesPG(t *testing.T) {
	in := []string{"PATH=/bin", "PGPASSWORD=secreto", "PGPASSFILE=/x", "PGHOST=db", "HOME=/h"}
	want := []string{"PATH=/bin", "PGHOST=db", "HOME=/h"}
	if got := sinClavesPG(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if in[1] != "PGPASSWORD=secreto" {
		t.Fatal("sinClavesPG no debe tocar el slice de entrada")
	}
}
