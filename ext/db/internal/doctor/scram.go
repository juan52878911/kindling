package doctor

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Verificadores SCRAM-SHA-256 tal como los guarda Postgres en
// pg_authid.rolpassword (RFC 5803):
//
//	SCRAM-SHA-256$<iteraciones>:<sal b64>$<StoredKey b64>:<ServerKey b64>
//
// Aquí solo hace falta comprobar si una contraseña conocida (la del dorado, o
// la que el host guardó para la copia) corresponde a un verificador.

// Tope de iteraciones: el verificador lo lee el invitado, que no es de fiar;
// uno con 2^31 iteraciones nos tendría calculando PBKDF2 una eternidad.
const verifierMaxIter = 1_000_000

type verifier struct {
	iter      int
	salt      []byte
	storedKey []byte
	serverKey []byte
}

var errNotSCRAM = errors.New("not a SCRAM-SHA-256 verifier")

func parseVerifier(s string) (*verifier, error) {
	rest, ok := strings.CutPrefix(s, "SCRAM-SHA-256$")
	if !ok {
		return nil, errNotSCRAM
	}
	params, keys, ok := strings.Cut(rest, "$")
	if !ok {
		return nil, errNotSCRAM
	}
	it, salt64, ok1 := strings.Cut(params, ":")
	st64, sv64, ok2 := strings.Cut(keys, ":")
	if !ok1 || !ok2 {
		return nil, errNotSCRAM
	}
	iter, err := strconv.Atoi(it)
	if err != nil || iter < 1 || iter > verifierMaxIter {
		return nil, errors.New("SCRAM verifier: iteration count out of range")
	}
	v := &verifier{iter: iter}
	if v.salt, err = base64.StdEncoding.DecodeString(salt64); err != nil || len(v.salt) == 0 {
		return nil, errNotSCRAM
	}
	if v.storedKey, err = base64.StdEncoding.DecodeString(st64); err != nil || len(v.storedKey) != sha256.Size {
		return nil, errNotSCRAM
	}
	if v.serverKey, err = base64.StdEncoding.DecodeString(sv64); err != nil || len(v.serverKey) != sha256.Size {
		return nil, errNotSCRAM
	}
	return v, nil
}

// matches dice si password produce este verificador. Sin SASLprep: las
// contraseñas de kling db son hex ASCII, que SASLprep deja igual.
func (v *verifier) matches(password string) bool {
	salted, err := pbkdf2.Key(sha256.New, password, v.salt, v.iter, sha256.Size)
	if err != nil {
		return false
	}
	ck := hmac.New(sha256.New, salted)
	ck.Write([]byte("Client Key"))
	stored := sha256.Sum256(ck.Sum(nil))
	sk := hmac.New(sha256.New, salted)
	sk.Write([]byte("Server Key"))
	return subtle.ConstantTimeCompare(stored[:], v.storedKey) == 1 &&
		subtle.ConstantTimeCompare(sk.Sum(nil), v.serverKey) == 1
}
