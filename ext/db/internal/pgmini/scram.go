package pgmini

// SCRAM-SHA-256 (RFC 5802 + RFC 7677) y su variante -PLUS con
// tls-server-end-point (RFC 5929). Es una copia de pkg/credproxy/scram.go
// (allí es privada y el núcleo y ext/db son módulos aparte); se mantienen sus
// mismas exigencias al servidor: nonce que extiende el nuestro, sal no vacía,
// iteraciones acotadas y firma del servidor comprobada.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
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
	// gs2 es la cabecera GS2: "n,," (sin channel binding), "y,," (lo
	// soportamos pero el servidor no ofreció -PLUS) o "p=tls-server-end-point,,".
	// Vacía vale "n,,".
	gs2            string
	cb             []byte // datos de channel binding con -PLUS
	primeroDesnudo string
	firmaServidor  []byte
}

func nuevoScram(clave string) (*scram, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &scram{clave: clave, nonce: base64.StdEncoding.EncodeToString(b[:])}, nil
}

// conBinding fija el modo de channel binding. cb son los datos
// tls-server-end-point del servidor (nil si no hay TLS o no se pueden
// calcular); plus, si el servidor ofrece -PLUS y se va a usar.
func (s *scram) conBinding(cb []byte, plus bool) {
	switch {
	case plus && cb != nil:
		s.gs2, s.cb = "p=tls-server-end-point,,", cb
	case cb != nil:
		// Un servidor que SÍ ofrecía -PLUS detecta con "y" que alguien lo
		// quitó de la lista por el camino (RFC 5802 §6).
		s.gs2 = "y,,"
	default:
		s.gs2 = "n,,"
	}
}

// primero es el client-first-message.
func (s *scram) primero() []byte {
	if s.gs2 == "" {
		s.gs2 = "n,,"
	}
	s.primeroDesnudo = "n=" + s.usuario + ",r=" + s.nonce
	return []byte(s.gs2 + s.primeroDesnudo)
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
	if _, ok := attrs['m']; ok {
		return nil, errors.New("scram: unsupported mandatory extension")
	}
	nonce := attrs['r']
	if !strings.HasPrefix(nonce, s.nonce) || len(nonce) <= len(s.nonce) {
		return nil, errors.New("scram: the server nonce does not extend ours")
	}
	for i := 0; i < len(nonce); i++ {
		if nonce[i] < 0x21 || nonce[i] > 0x7e || nonce[i] == ',' {
			return nil, errors.New("scram: invalid server nonce")
		}
	}
	sal, err := base64.StdEncoding.DecodeString(attrs['s'])
	if err != nil || len(sal) == 0 {
		return nil, errors.New("scram: invalid salt")
	}
	iter, err := strconv.Atoi(attrs['i'])
	if err != nil || iter < scramMinIter || iter > scramMaxIter {
		return nil, fmt.Errorf("scram: iteration count outside %d..%d", scramMinIter, scramMaxIter)
	}
	cbind := base64.StdEncoding.EncodeToString(append([]byte(s.gs2), s.cb...)) // "n,," -> biws
	sinPrueba := "c=" + cbind + ",r=" + nonce
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

// tlsServerEndPoint son los datos de channel binding tls-server-end-point
// (RFC 5929 §4.1): el hash del certificado del servidor con el algoritmo de su
// firma, con MD5 y SHA-1 subidos a SHA-256. nil si la firma no usa un hash
// (Ed25519): entonces no hay channel binding, igual que en el servidor.
func tlsServerEndPoint(cert *x509.Certificate) []byte {
	var h hash.Hash
	switch cert.SignatureAlgorithm {
	case x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1, x509.DSAWithSHA1,
		x509.SHA256WithRSA, x509.ECDSAWithSHA256, x509.SHA256WithRSAPSS, x509.DSAWithSHA256:
		h = sha256.New()
	case x509.SHA384WithRSA, x509.ECDSAWithSHA384, x509.SHA384WithRSAPSS:
		h = sha512.New384()
	case x509.SHA512WithRSA, x509.ECDSAWithSHA512, x509.SHA512WithRSAPSS:
		h = sha512.New()
	default:
		return nil
	}
	h.Write(cert.Raw)
	return h.Sum(nil)
}
