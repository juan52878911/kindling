package main

import (
	"context"
	"github.com/juan52878911/kindling/pkg/api"
	"strings"
	"time"
)

// waitGuest espera a que el servidor de dentro de la microVM abra su puerto.
// La espera ocurre en el daemon: las IP de los invitados no son alcanzables
// desde un CLI remoto, y sondearlas desde aquí falla siempre por SSH.
func waitGuest(ctx context.Context, c *api.Client, ref string, timeout time.Duration) error {
	_, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: 8080, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	})
	return err
}

// splitDomains parte una lista de dominios separada por comas, recortando
// espacios y descartando vacíos. Acepta también espacios como separador para
// tolerar "dom1, dom2".
func splitDomains(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if d := strings.TrimSpace(f); d != "" {
			out = append(out, d)
		}
	}
	return out
}
