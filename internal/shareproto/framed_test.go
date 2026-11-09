package share

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// Una respuesta de ResponseFrame rellenada con BytesFrom sale por
// WriteFramed igual que la de Response + Bytes + WriteFrame, sin crecer.
func TestRespuestaEnUnSoloBufer(t *testing.T) {
	datos := bytes.Repeat([]byte("kindling"), 1000)
	for _, k := range []int{len(datos), 100, 0} {
		r := ResponseFrame(7, 0, 4+len(datos))
		cap0 := cap(r.B)
		err := r.BytesFrom(len(datos), func(b []byte) (int, error) {
			n := copy(b, datos[:k])
			if k < len(datos) {
				return n, io.EOF
			}
			return n, nil
		})
		if (k < len(datos)) != errors.Is(err, io.EOF) {
			t.Fatalf("k=%d: err %v", k, err)
		}
		if cap(r.B) != cap0 {
			t.Errorf("k=%d: the buffer grew from %d to %d", k, cap0, cap(r.B))
		}
		var got bytes.Buffer
		if err := WriteFramed(&got, r.B); err != nil {
			t.Fatal(err)
		}
		want := Response(7, 0)
		want.Bytes(datos[:k])
		var wantFrame bytes.Buffer
		if err := WriteFrame(&wantFrame, want.B); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), wantFrame.Bytes()) {
			t.Fatalf("k=%d: framed reply differs from WriteFrame's", k)
		}
	}
	if err := WriteFramed(io.Discard, make([]byte, FrameHead+MaxFrame+1)); err == nil {
		t.Error("a frame over MaxFrame was written")
	}
	if err := WriteFramed(io.Discard, nil); err == nil {
		t.Error("a frame without header room was written")
	}
}
