package net

import (
	"encoding/binary"
	"io"
	stdnet "net"
	"sync"
	"testing"
)

// fakeUpstreamTCP es fakeUpstream en TCP (DNS con prefijo de longitud).
func fakeUpstreamTCP(t *testing.T, handle func(query []byte) []byte) string {
	t.Helper()
	ln, err := stdnet.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeUpstreamTCP: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp := handle(q)
				binary.BigEndian.PutUint16(l[:], uint16(len(resp)))
				_, _ = c.Write(append(l[:], resp...))
			}()
		}
	}()
	return ln.Addr().String()
}

// TestTxidDelInvitadoNoSaleDelHost: el upstream ve un TXID que no es el que
// eligió el invitado, y el invitado recibe la respuesta con el suyo. Vale para
// UDP y TCP.
func TestTxidDelInvitadoNoSaleDelHost(t *testing.T) {
	restore := fakeEjecutar(nil)
	defer restore()
	const delInvitado = 0x1234
	for _, viaTCP := range []bool{false, true} {
		var mu sync.Mutex
		vistos := map[uint16]int{}
		eco := func(q []byte) []byte {
			mu.Lock()
			vistos[binary.BigEndian.Uint16(q[0:2])]++
			mu.Unlock()
			return buildAnswerFor(q, "1.2.3.4", 300) // contesta con el txid recibido
		}
		var addr string
		if viaTCP {
			addr = fakeUpstreamTCP(t, eco)
		} else {
			var closeFn func()
			addr, closeFn = fakeUpstream(t, eco)
			defer closeFn()
		}
		r := newTestResolver(addr)
		for i := 0; i < 8; i++ {
			resp := r.process(buildQuery(delInvitado, "example.com", 1), viaTCP)
			if len(resp) < 12 || resp[3]&0x0F != 0 {
				t.Fatalf("tcp=%v: esperaba respuesta válida, obtuve %v", viaTCP, resp)
			}
			if got := binary.BigEndian.Uint16(resp[0:2]); got != delInvitado {
				t.Fatalf("tcp=%v: el invitado recibe txid %#x, quería el suyo %#x", viaTCP, got, delInvitado)
			}
		}
		mu.Lock()
		if vistos[delInvitado] == 8 || len(vistos) < 2 {
			t.Fatalf("tcp=%v: el upstream vio los txid %v: el del invitado sale tal cual", viaTCP, vistos)
		}
		mu.Unlock()
	}
}

// TestRespuestaConTxidDelInvitadoSeRechaza: un spoofer que conoce el TXID que
// eligió el invitado (porque controla el invitado) ya no acierta: la
// respuesta con ese TXID da SERVFAIL y no siembra nada.
func TestRespuestaConTxidDelInvitadoSeRechaza(t *testing.T) {
	var calls int32
	restore := fakeEjecutar(&calls)
	defer restore()
	const delInvitado = 0x4242
	spoof := func(q []byte) []byte {
		return buildAnswerFor(buildQuery(delInvitado, "example.com", 1), "6.6.6.6", 300)
	}
	addrUDP, closeFn := fakeUpstream(t, spoof)
	defer closeFn()
	addrTCP := fakeUpstreamTCP(t, spoof)
	for _, c := range []struct {
		addr   string
		viaTCP bool
	}{{addrUDP, false}, {addrTCP, true}} {
		r := newTestResolver(c.addr)
		resp := r.process(buildQuery(delInvitado, "example.com", 1), c.viaTCP)
		if len(resp) < 4 || resp[3]&0x0F != 2 {
			t.Fatalf("tcp=%v: esperaba SERVFAIL ante una respuesta con el txid del invitado, obtuve %v", c.viaTCP, resp)
		}
	}
	if calls != 0 {
		t.Fatalf("sembró el ipset con una respuesta falsa: %d llamadas", calls)
	}
}
