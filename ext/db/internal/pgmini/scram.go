package pgmini

// SCRAM-SHA-256 (RFC 5802 + RFC 7677) sin channel binding: lo justo para
// autenticarse contra un PostgreSQL sin TLS. Es una copia reducida de
// pkg/credproxy/scram.go (allí es privada y trae -PLUS); se mantienen sus
// mismas exigencias al servidor: nonce que extiende el nuestro, sal no vacía,
// iteraciones acotadas y firma del servidor comprobada.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	scramMinIter = 4096
	scramMaxIter = 1_000_000
	scramMaxMsg  = 2048
)

type scram struct {
	clave, usuario, nonce string
	primeroDesnudo        string
	firmaServidor         []byte
}

func nuevoScram(clave string) (*scram, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &scram{clave: clave, nonce: base64.StdEncoding.EncodeToString(b[:])}, nil
}

// primero es el client-first-message ("n,," = sin channel binding).
func (s *scram) primero() []byte {
	s.primeroDesnudo = "n=" + s.usuario + ",r=" + s.nonce
	return []byte("n,," + s.primeroDesnudo)
}

// final recibe el server-first-message y devuelve el client-final-message.
func (s *scram) final(servidor []byte) ([]byte, error) {
	if len(servidor) > scramMaxMsg {
		return nil, errors.New("scram: server-first-message too long")
	}
	sf := string(servidor)
	attrs, err := atributos(sf)
	if err != nil {
		return nil, err
	}
	nonce := attrs['r']
	if !strings.HasPrefix(nonce, s.nonce) || len(nonce) <= len(s.nonce) {
		return nil, errors.New("scram: the server nonce does not extend ours")
	}
	sal, err := base64.StdEncoding.DecodeString(attrs['s'])
	if err != nil || len(sal) == 0 {
		return nil, errors.New("scram: invalid salt")
	}
	iter, err := strconv.Atoi(attrs['i'])
	if err != nil || iter < scramMinIter || iter > scramMaxIter {
		return nil, fmt.Errorf("scram: iteration count outside %d..%d", scramMinIter, scramMaxIter)
	}
	sinPrueba := "c=biws,r=" + nonce // biws = base64("n,,")
	authMsg := s.primeroDesnudo + "," + sf + "," + sinPrueba

	salted, err := pbkdf2.Key(sha256.New, s.clave, sal, iter, sha256.Size)
	if err != nil {
		return nil, err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	firmaCliente := hmacSHA256(storedKey[:], authMsg)
	prueba := make([]byte, len(clientKey))
	for i := range clientKey {
		prueba[i] = clientKey[i] ^ firmaCliente[i]
	}
	s.firmaServidor = hmacSHA256(hmacSHA256(salted, "Server Key"), authMsg)
	return []byte(sinPrueba + ",p=" + base64.StdEncoding.EncodeToString(prueba)), nil
}

// verificar comprueba el server-final-message.
func (s *scram) verificar(servidor []byte) error {
	if s.firmaServidor == nil {
		return errors.New("scram: server-final-message before client-final")
	}
	if len(servidor) > scramMaxMsg {
		return errors.New("scram: server-final-message too long")
	}
	attrs, err := atributos(string(servidor))
	if err != nil {
		return err
	}
	if _, ok := attrs['e']; ok {
		return errors.New("scram: the server rejected the proof")
	}
	v, err := base64.StdEncoding.DecodeString(attrs['v'])
	if err != nil || subtle.ConstantTimeCompare(v, s.firmaServidor) != 1 {
		return errors.New("scram: invalid server signature")
	}
	return nil
}

func atributos(m string) (map[byte]string, error) {
	out := map[byte]string{}
	for _, p := range strings.Split(m, ",") {
		if len(p) < 2 || p[1] != '=' {
			return nil, errors.New("scram: malformed message")
		}
		if _, dup := out[p[0]]; dup {
			return nil, errors.New("scram: repeated attribute")
		}
		out[p[0]] = p[2:]
	}
	return out, nil
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
