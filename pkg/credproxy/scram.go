package credproxy

// Cliente SCRAM-SHA-256 (RFC 5802 + RFC 7677) y su variante -PLUS con
// tls-server-end-point (RFC 5929), lo justo para autenticarse contra un
// PostgreSQL. Propio y con la biblioteca estándar: el módulo no tiene
// dependencias y vz/ lo compila también.
//
// Lo que se exige del servidor, porque el proxy habla con la clave real:
//   - el nonce del servidor empieza por el nuestro y lo alarga;
//   - la sal no está vacía y las iteraciones caen en [scramMinIter, scramMaxIter]:
//     por debajo la prueba que mandamos se rompe barata por fuerza bruta, por
//     encima un servidor hostil nos haría quemar CPU;
//   - la firma del servidor (v=) se comprueba antes de dar la autenticación
//     por buena: sin ella, un servidor que no conoce la clave podría
//     hacerse pasar por el bueno.
//
// La contraseña se usa tal cual, sin SASLprep: ValidarCredenciales solo admite
// ASCII imprimible para Postgres, y para eso SASLprep es la identidad.

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
	scramSHA256     = "SCRAM-SHA-256"
	scramSHA256Plus = "SCRAM-SHA-256-PLUS"
	scramMinIter    = 4096
	scramMaxIter    = 1_000_000
	// scramMaxMsg acota lo que se acepta de un mensaje del servidor.
	scramMaxMsg = 2048
)

// scramCliente es el estado de un intercambio.
type scramCliente struct {
	clave string
	// usuario va en el client-first; PostgreSQL lo ignora (usa el del
	// StartupMessage) y libpq lo manda vacío. Campo aparte para el vector de
	// la RFC 7677.
	usuario string
	nonce   string
	gs2     string // cabecera GS2: "n,,", "y,," o "p=tls-server-end-point,,"
	cb      []byte // datos de channel binding con -PLUS

	primeroDesnudo string
	firmaServidor  []byte
}

// nuevoScram prepara el intercambio. cb son los datos tls-server-end-point
// del servidor (nil si no se pudieron calcular); plus, si el servidor ofrece
// -PLUS y se va a usar.
func nuevoScram(clave string, cb []byte, plus bool) (*scramCliente, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	s := &scramCliente{clave: clave, nonce: base64.StdEncoding.EncodeToString(b[:])}
	switch {
	case plus:
		s.gs2, s.cb = "p=tls-server-end-point,,", cb
	case cb != nil:
		// Soportamos channel binding pero el servidor no ofreció -PLUS: "y"
		// se lo dice, y un servidor que SÍ lo ofrecía detecta que alguien
		// quitó -PLUS de la lista por el camino (RFC 5802 §6).
		s.gs2 = "y,,"
	default:
		s.gs2 = "n,,"
	}
	return s, nil
}

// primero es el client-first-message.
func (s *scramCliente) primero() []byte {
	s.primeroDesnudo = "n=" + s.usuario + ",r=" + s.nonce
	return []byte(s.gs2 + s.primeroDesnudo)
}

// final recibe el server-first-message y devuelve el client-final-message.
func (s *scramCliente) final(servidor []byte) ([]byte, error) {
	if len(servidor) > scramMaxMsg {
		return nil, errors.New("scram: server-first-message too long")
	}
	sf := string(servidor)
	attrs, err := scramAtributos(sf)
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

	cbind := base64.StdEncoding.EncodeToString(append([]byte(s.gs2), s.cb...))
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
	serverKey := hmacSHA256(salted, "Server Key")
	s.firmaServidor = hmacSHA256(serverKey, authMsg)
	return []byte(sinPrueba + ",p=" + base64.StdEncoding.EncodeToString(prueba)), nil
}

// verificar comprueba el server-final-message. Solo tras un nil se puede dar
// por buena la autenticación.
func (s *scramCliente) verificar(servidor []byte) error {
	if s.firmaServidor == nil {
		return errors.New("scram: server-final-message before client-final")
	}
	if len(servidor) > scramMaxMsg {
		return errors.New("scram: server-final-message too long")
	}
	attrs, err := scramAtributos(string(servidor))
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

// scramAtributos parte "a=valor,b=valor" en un mapa. Un atributo repetido o
// mal formado es un error.
func scramAtributos(m string) (map[byte]string, error) {
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
