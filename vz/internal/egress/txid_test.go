package egress

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// upstreamFalso contesta por UDP y TCP en 127.0.0.1 y guarda el TXID de cada
// consulta. Con fijo != 0 contesta siempre con ese TXID en vez del recibido.
type upstreamFalso struct {
	addr string
	fijo uint16
	mu   sync.Mutex
	vist []uint16
}

func (u *upstreamFalso) responder(q []byte) []byte {
	u.mu.Lock()
	u.vist = append(u.vist, binary.BigEndian.Uint16(q[0:2]))
	u.mu.Unlock()
	resp := append([]byte(nil), q...)
	resp[2] |= 0x80
	if u.fijo != 0 {
		binary.BigEndian.PutUint16(resp[0:2], u.fijo)
	}
	return resp
}

func nuevoUpstream(t *testing.T, fijo uint16) *upstreamFalso {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		pc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close(); ln.Close() })
	u := &upstreamFalso{addr: pc.LocalAddr().String(), fijo: fijo}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(u.responder(buf[:n]), from)
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if q, err := ReadTCPMsg(c); err == nil {
					_ = WriteTCPMsg(c, u.responder(q))
				}
			}()
		}
	}()
	return u
}

// El TXID del invitado no llega al upstream: el resolver de macOS pone uno
// propio y devuelve al invitado el suyo, como el de Linux. Una respuesta que
// no trae el TXID propio se descarta.
func TestExchangeTxidPropio(t *testing.T) {
	const delInvitado = 0x1234
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, viaTCP := range []bool{false, true} {
		u := nuevoUpstream(t, 0)
		iguales := 0
		const n = 4
		for i := 0; i < n; i++ {
			q := BuildQuery("example.com", 1)
			binary.BigEndian.PutUint16(q[0:2], delInvitado)
			resp, err := exchange(ctx, u.addr, q, viaTCP)
			if err != nil {
				t.Fatalf("tcp=%v: %v", viaTCP, err)
			}
			if !ResponseMatches(q, resp) {
				t.Fatalf("tcp=%v: la respuesta no lleva el TXID del invitado", viaTCP)
			}
		}
		u.mu.Lock()
		for _, v := range u.vist {
			if v == delInvitado {
				iguales++
			}
		}
		u.mu.Unlock()
		if iguales == n {
			t.Fatalf("tcp=%v: el upstream vio el TXID del invitado en las %d consultas", viaTCP, n)
		}

		ajeno := nuevoUpstream(t, delInvitado)
		q := BuildQuery("example.com", 1)
		binary.BigEndian.PutUint16(q[0:2], delInvitado)
		ok := 0
		for i := 0; i < n; i++ {
			if _, err := exchange(ctx, ajeno.addr, q, viaTCP); err == nil {
				ok++
			}
		}
		if ok == n {
			t.Fatalf("tcp=%v: una respuesta con el TXID del invitado se aceptó %d veces", viaTCP, n)
		}
	}
}
