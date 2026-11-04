package share

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	validos := []string{
		"a", "...", ".a", "a.", "..a", "a b", "a\\b", "a:b", "-", "ñandú", "日本語",
		strings.Repeat("x", MaxName),
		// El tope es en bytes, no en runas: 127 «ñ» son 254 bytes.
		strings.Repeat("ñ", 127),
	}
	for _, n := range validos {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q): %v", n, err)
		}
	}
	invalidos := []string{
		"", ".", "..", "/", "a/b", "/a", "a/", "../a", "a\x00", "\x00",
		strings.Repeat("x", MaxName+1),
		strings.Repeat("ñ", 128), // 256 bytes
	}
	for _, n := range invalidos {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) aceptado", n)
		}
	}
}

// trama es una cabecera con n y el cuerpo dado (que puede no medir n).
func trama(n uint32, cuerpo []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, n)
	return append(b, cuerpo...)
}

func TestReadFrameLimit(t *testing.T) {
	tope := func(n int) func() int { return func() int { return n } }
	casos := []struct {
		nombre string
		datos  []byte
		limite int
		quiero []byte
		err    error // nil = sin error; errTope = el del tope
	}{
		{"vacía", trama(0, nil), 10, []byte{}, nil},
		{"justo en el tope", trama(10, bytes.Repeat([]byte("a"), 10)), 10, bytes.Repeat([]byte("a"), 10), nil},
		{"un byte de más", trama(11, bytes.Repeat([]byte("a"), 11)), 10, nil, errTope},
		{"cabecera enorme sin cuerpo", trama(0xffffffff, nil), MaxFrame, nil, errTope},
		{"tope cero", trama(1, []byte("a")), 0, nil, errTope},
		{"tope negativo", trama(0, nil), -1, nil, errTope},
		{"sin nada", nil, 10, nil, io.EOF},
		{"cabecera cortada", []byte{0, 0}, 10, nil, io.ErrUnexpectedEOF},
		{"cuerpo cortado", trama(5, []byte("ab")), 10, nil, io.ErrUnexpectedEOF},
		{"cuerpo sin empezar", trama(5, nil), 10, nil, io.EOF},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			r := bytes.NewReader(c.datos)
			got, err := ReadFrameLimit(r, tope(c.limite))
			switch {
			case c.err == errTope:
				if err == nil || !strings.Contains(err.Error(), "the limit is") {
					t.Fatalf("err = %v, quiero el del tope", err)
				}
				// Rechazada por la cabecera: el cuerpo ni se lee ni se reserva.
				if consumido := len(c.datos) - r.Len(); consumido != 4 {
					t.Errorf("leyó %d bytes; con el tope superado solo se lee la cabecera", consumido)
				}
			case c.err != nil:
				if !errors.Is(err, c.err) {
					t.Fatalf("err = %v, quiero %v", err, c.err)
				}
			default:
				if err != nil || !bytes.Equal(got, c.quiero) || got == nil {
					t.Fatalf("got %q, %v; quiero %q", got, err, c.quiero)
				}
			}
		})
	}
}

// El tope se pregunta después de leer la cabecera, en cada trama: quien
// negocia puede subirlo entre una y la siguiente.
func TestReadFrameLimitConsultaElTopeTrasLaCabecera(t *testing.T) {
	var flujo bytes.Buffer
	flujo.Write(trama(4, []byte("hola")))
	flujo.Write(trama(8, []byte("12345678")))
	r := bytes.NewReader(flujo.Bytes())
	limite, llamadas := 4, 0
	limit := func() int {
		llamadas++
		if r.Len() != flujo.Len()-4 && r.Len() != flujo.Len()-12 {
			t.Errorf("tope consultado con %d bytes por leer: no justo tras una cabecera", r.Len())
		}
		return limite
	}
	if b, err := ReadFrameLimit(r, limit); err != nil || string(b) != "hola" {
		t.Fatalf("primera: %q, %v", b, err)
	}
	limite = 8
	if b, err := ReadFrameLimit(r, limit); err != nil || string(b) != "12345678" {
		t.Fatalf("segunda con el tope subido: %q, %v", b, err)
	}
	if llamadas != 2 {
		t.Errorf("tope consultado %d veces, quiero una por trama", llamadas)
	}
}

// Lo que WriteFrame escribe con el tope de ReadFrame, ReadFrame lo lee, y
// lo que no se puede escribir tampoco se acepta al leer.
func TestTramasEnElTope(t *testing.T) {
	var buf bytes.Buffer
	cuerpo := bytes.Repeat([]byte{0xab}, MaxFrame)
	if err := WriteFrame(&buf, cuerpo); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFrame(&buf); err != nil || !bytes.Equal(b, cuerpo) {
		t.Fatalf("trama máxima: %d bytes, %v", len(b), err)
	}
	if err := WriteFrame(&buf, make([]byte, MaxFrame+1)); err == nil || buf.Len() != 0 {
		t.Fatalf("trama de más: err %v, %d bytes escritos", err, buf.Len())
	}
	if _, err := ReadFrame(bytes.NewReader(trama(MaxFrame+1, nil))); err == nil {
		t.Fatal("ReadFrame aceptó una cabecera de MaxFrame+1")
	}
}

var errTope = errors.New("tope")
