package mysqlpw

import "testing"

// Referencias de `openssl passwd -5` y del crypt() de glibc ("$5$", 5000
// rondas por defecto): el algoritmo es ese, salvo que MySQL no corta la sal a 16.
func TestSHA256CryptVectoresDrepper(t *testing.T) {
	for _, c := range []struct{ pw, salt, want string }{
		{"Hello world!", "saltstring", "5B8vYYiY.CVt1RlTTf8KbXBH3hsxY/GNooZaBBGWEc5"},
		{"This is just a test", "toolongsaltstrin", "Un/5jzAHMgOGZ5.mWJpuVolil07guHPvOW8mGRcvxa5"},
	} {
		if got := string(sha256Crypt([]byte(c.pw), []byte(c.salt), 5000)); got != c.want {
			t.Errorf("sha256Crypt(%q, %q) = %s, quiero %s", c.pw, c.salt, got, c.want)
		}
	}
}

func TestCachingSHA2Hash(t *testing.T) {
	h, err := NewCachingSHA2Hash("clave de prueba")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidCachingSHA2Hash(h) || len(h) != 7+20+43 {
		t.Fatalf("hash con forma rara: %q", h)
	}
	if !CheckCachingSHA2("clave de prueba", h) || CheckCachingSHA2("otra", h) {
		t.Fatal("CheckCachingSHA2 no reconoce su propio hash, o acepta otra clave")
	}
	h2, _ := NewCachingSHA2Hash("clave de prueba")
	if h == h2 {
		t.Fatal("dos hashes de la misma clave con la misma sal")
	}
	for _, malo := range []string{"", "*" + "A", "$A$005$corto", "$A$010$" + h[7:], "$A$005$" + h[7:66] + "'"} {
		if ValidCachingSHA2Hash(malo) {
			t.Errorf("ValidCachingSHA2Hash(%q) = true", malo)
		}
	}
}
