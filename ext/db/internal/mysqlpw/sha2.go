package mysqlpw

// caching_sha2_password (el método por defecto de MySQL 8.4 y el único de
// MySQL 9, que ya no trae mysql_native_password) guarda en
// authentication_string:
//
//	$A$005$<sal de 20 bytes><43 caracteres del SHA-256-crypt>
//
// "005" son las rondas en miles (5000). El resumen es el SHA-256-crypt de
// Drepper (el "$5$" de glibc), pero con los 20 bytes de sal enteros: el
// estándar los corta a 16 y MySQL no. La sal la elige quien calcula el hash;
// aquí sale del alfabeto de crypt ([./0-9A-Za-z]), así que el hash entero va
// entre comillas simples en SQL sin escapar nada. Validado contra un servidor
// MySQL 8.4 real (docs/mysql.md).

import (
	"crypto/rand"
	"crypto/sha256"
	"regexp"
)

// CachingSHA2Plugin es el nombre del plugin cuyo hash da CachingSHA2Hash.
const CachingSHA2Plugin = "caching_sha2_password"

const (
	sha2Rounds  = 5000
	sha2SaltLen = 20
	cryptB64    = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// reSHA2 es la forma de un hash de caching_sha2_password con sal del
// alfabeto de crypt: los que da CachingSHA2Hash.
var reSHA2 = regexp.MustCompile(`^\$A\$005\$[./0-9A-Za-z]{63}$`)

// ValidCachingSHA2Hash dice si h tiene la forma de un hash que kling db sabe
// comprobar y meter en SQL sin escapar.
func ValidCachingSHA2Hash(h string) bool { return reSHA2.MatchString(h) }

// NewCachingSHA2Hash es el authentication_string de caching_sha2_password
// para pw, con una sal nueva.
func NewCachingSHA2Hash(pw string) (string, error) {
	var b [sha2SaltLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	salt := make([]byte, sha2SaltLen)
	for i, c := range b {
		salt[i] = cryptB64[int(c)%len(cryptB64)]
	}
	return CachingSHA2Hash(pw, salt), nil
}

// CachingSHA2Hash es el authentication_string para pw con la sal dada.
func CachingSHA2Hash(pw string, salt []byte) string {
	return "$A$005$" + string(salt) + string(sha256Crypt([]byte(pw), salt, sha2Rounds))
}

// CheckCachingSHA2 dice si h es el hash de caching_sha2_password de pw.
func CheckCachingSHA2(pw, h string) bool {
	if !ValidCachingSHA2Hash(h) {
		return false
	}
	salt := []byte(h[7 : 7+sha2SaltLen])
	return CachingSHA2Hash(pw, salt) == h
}

// sha256Crypt es el SHA-256-crypt de Drepper (glibc, "$5$") sin cortar la
// sal, ya codificado: 43 caracteres del alfabeto de crypt.
func sha256Crypt(pw, salt []byte, rounds int) []byte {
	sum := func(parts ...[]byte) []byte {
		h := sha256.New()
		for _, p := range parts {
			h.Write(p)
		}
		return h.Sum(nil)
	}
	b := sum(pw, salt, pw)

	a := sha256.New()
	a.Write(pw)
	a.Write(salt)
	n := len(pw)
	for ; n > 32; n -= 32 {
		a.Write(b)
	}
	a.Write(b[:n])
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			a.Write(b)
		} else {
			a.Write(pw)
		}
	}
	c := a.Sum(nil)

	dp := sha256.New()
	for range pw {
		dp.Write(pw)
	}
	p := repetir(dp.Sum(nil), len(pw))

	ds := sha256.New()
	for i := 0; i < 16+int(c[0]); i++ {
		ds.Write(salt)
	}
	s := repetir(ds.Sum(nil), len(salt))

	for r := 0; r < rounds; r++ {
		h := sha256.New()
		if r&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if r%3 != 0 {
			h.Write(s)
		}
		if r%7 != 0 {
			h.Write(p)
		}
		if r&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(c[:0])
	}

	out := make([]byte, 0, 43)
	b64 := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for ; n > 0; n-- {
			out = append(out, cryptB64[w&0x3f])
			w >>= 6
		}
	}
	for _, t := range [][3]int{{0, 10, 20}, {21, 1, 11}, {12, 22, 2}, {3, 13, 23}, {24, 4, 14},
		{15, 25, 5}, {6, 16, 26}, {27, 7, 17}, {18, 28, 8}, {9, 19, 29}} {
		b64(c[t[0]], c[t[1]], c[t[2]], 4)
	}
	b64(0, c[31], c[30], 3)
	return out
}

// repetir son los primeros n bytes de d repetido.
func repetir(d []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, d[:min(len(d), n-len(out))]...)
	}
	return out
}
