package egress

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

// La caché: un nombre repetido no vuelve al upstream mientras dure el menor
// TTL de la respuesta (acotado por MaxCacheTTL), con el id de cada consulta;
// TTL 0 no se guarda; una respuesta vacía dura NegCacheTTL; al caducar se
// vuelve a preguntar. En allowlist cada acierto siembra igual la IP.
func TestResolverCache(t *testing.T) {
	p := NewPolicy()
	p.Set(Allowlist, []string{"example.com"})
	now := time.Unix(1000, 0)
	p.IPSet().now = func() time.Time { return now }
	var forwarded int
	ttl := uint32(20)
	r := &Resolver{Policy: p, ahora: func() time.Time { return now }, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, ttl, "93.184.215.14"), nil
	}}
	q1 := BuildQuery("www.example.com", 1)
	q2 := BuildQuery("WWW.Example.COM", 1)
	binary.BigEndian.PutUint16(q2[0:2], 0xbeef)
	r1 := r.Process(context.Background(), q1, false)
	r2 := r.Process(context.Background(), q2, false)
	if forwarded != 1 {
		t.Fatalf("forwarded %d times; the second query must come from the cache", forwarded)
	}
	if binary.BigEndian.Uint16(r2[0:2]) != 0xbeef || len(r2) != len(r1) {
		t.Fatalf("cached answer: id %#x len %d; want the query's id %#x and the same body (%d)", r2[0:2], len(r2), 0xbeef, len(r1))
	}
	if !p.AllowConn(netip.MustParseAddr("93.184.215.14")) {
		t.Fatal("a cached answer must seed the allowlist too")
	}
	now = now.Add(21 * time.Second)
	r.Process(context.Background(), q1, false)
	if forwarded != 2 {
		t.Fatalf("after the TTL the name must be forwarded again (forwarded %d)", forwarded)
	}
	ttl = 0
	q3 := BuildQuery("zero.example.com", 1)
	r.Process(context.Background(), q3, false)
	r.Process(context.Background(), q3, false)
	if forwarded != 4 {
		t.Fatalf("TTL 0 must not be cached (forwarded %d)", forwarded)
	}
	// Vacía (AAAA de un nombre solo IPv4): NegCacheTTL.
	r.Exchange = func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, 0), nil
	}
	q4 := BuildQuery("www.example.com", 28)
	r.Process(context.Background(), q4, false)
	r.Process(context.Background(), q4, false)
	if forwarded != 5 {
		t.Fatalf("an empty NOERROR must be cached (forwarded %d)", forwarded)
	}
	now = now.Add(NegCacheTTL + time.Second)
	r.Process(context.Background(), q4, false)
	if forwarded != 6 {
		t.Fatalf("an empty answer must expire after NegCacheTTL (forwarded %d)", forwarded)
	}
	// Un TTL enorme se acota a MaxCacheTTL.
	r.Exchange = func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, 86400, "93.184.215.14"), nil
	}
	q5 := BuildQuery("long.example.com", 1)
	r.Process(context.Background(), q5, false)
	now = now.Add(MaxCacheTTL + time.Second)
	r.Process(context.Background(), q5, false)
	if forwarded != 8 {
		t.Fatalf("a day-long TTL must be capped at MaxCacheTTL (forwarded %d)", forwarded)
	}
	// Una respuesta truncada no se guarda.
	r.Exchange = func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		resp := answer(q, 60, "93.184.215.14")
		resp[2] |= 0x02 // TC
		return resp, nil
	}
	q6 := BuildQuery("tc.example.com", 1)
	r.Process(context.Background(), q6, false)
	r.Process(context.Background(), q6, false)
	if forwarded != 10 {
		t.Fatalf("a truncated answer must not be cached (forwarded %d)", forwarded)
	}
}

// La caché no crece sin tope: con MaxCacheEntries nombres distintos entra
// uno más y sigue habiendo como mucho MaxCacheEntries.
func TestResolverCacheTope(t *testing.T) {
	p := NewPolicy()
	p.Set(Internet, nil)
	r := &Resolver{Policy: p, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		return answer(q, 60, "93.184.215.14"), nil
	}}
	for i := 0; i <= MaxCacheEntries; i++ {
		r.Process(context.Background(), BuildQuery(fmt.Sprintf("h%d.example.com", i), 1), false)
	}
	if n := len(r.cache); n > MaxCacheEntries {
		t.Fatalf("cache holds %d entries, cap is %d", n, MaxCacheEntries)
	}
}
