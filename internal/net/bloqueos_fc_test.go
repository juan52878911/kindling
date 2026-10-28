//go:build !darwin

package net

import (
	"strings"
	"testing"
)

// Salida real de `nft -j list set` (nftables 1.0.6) con un elemento.
const setNftReal = `{"nftables": [{"metainfo": {"version": "1.0.6"}}, {"set": {"family": "ip", "name": "d", "table": "kl_bloq", "type": ["ipv4_addr", "inet_proto", "inet_service"], "handle": 2, "size": 64, "flags": ["timeout"], "timeout": 3600, "elem": [{"elem": {"val": {"concat": ["93.184.216.34", "tcp", 8080]}, "expires": 3598, "counter": {"packets": 3, "bytes": 180}}}, {"elem": {"val": {"concat": ["8.8.8.8", "udp", 53]}, "counter": {"packets": 0, "bytes": 0}}}], "stmt": [{"counter": null}]}}]}`

func TestParseSetNft(t *testing.T) {
	got := parseSetNft([]byte(setNftReal))
	if len(got) != 2 || got[0] != (Destino{"93.184.216.34:8080", "ip", 3}) ||
		got[1] != (Destino{"8.8.8.8:53/udp", "ip", 1}) {
		t.Fatalf("got %v", got)
	}
	// Un set vacío o basura no rompe nada.
	for _, in := range []string{``, `no es json`, `{"nftables": []}`,
		`{"nftables":[{"set":{"elem":[{"elem":{"val":{"concat":["x"]}}}]}}]}`} {
		if got := parseSetNft([]byte(in)); len(got) != 0 {
			t.Fatalf("%q: %v", in, got)
		}
	}
}

// El script de nft excluye lo que va al host (DNS, proxies), acota el set y
// no guarda nada del contenido.
func TestReglasRegistroIP(t *testing.T) {
	n := Plan(5, "abcdef123456")
	s := n.reglasRegistroIP()
	for _, want := range []string{"ip daddr != " + n.HostIP, "ct state new", "size ", "timeout 1h", `iifname "tap0"`} {
		if !strings.Contains(s, want) {
			t.Errorf("falta %q en:\n%s", want, s)
		}
	}
}
