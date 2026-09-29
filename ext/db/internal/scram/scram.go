// Package scram calcula lo que Postgres guarda de una contraseña SCRAM-SHA-256
// (RFC 5802, RFC 7677) y el formato de su verificador (RFC 5803):
//
//	SCRAM-SHA-256$<iteraciones>:<sal b64>$<StoredKey b64>:<ServerKey b64>
//
// Sirve para rotar la contraseña de una base sin que la clave llegue nunca al
// invitado: el host la genera, calcula el verificador y manda SOLO el
// verificador en un `ALTER ROLE ... PASSWORD '<verificador>'`. Postgres lo
// reconoce como ya cifrado y lo guarda tal cual. Con el verificador se puede
// hacer de servidor, pero no autenticarse como cliente: para eso hace falta
// ClientKey, que solo sale de la contraseña.
package scram

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	// Iterations es lo que usa Postgres por defecto (scram_iterations).
	Iterations = 4096
	// SaltLen es la longitud de la sal que genera Postgres.
	SaltLen = 16
	// PasswordBytes es la entropía de GeneratePassword.
	PasswordBytes = 32
)

// Keys son las tres claves de SCRAM que salen de la contraseña.
type Keys struct {
	ClientKey, StoredKey, ServerKey []byte
}

// Derive calcula las claves de password con esa sal e iteraciones.
//
// No aplica SASLprep: solo acepta ASCII imprimible, para el que SASLprep es la
// identidad. Así el verificador coincide con el que calcularía Postgres, que
// sí lo aplica, sin tener que implementarlo.
func Derive(password string, salt []byte, iter int) (Keys, error) {
	if password == "" {
		return Keys{}, errors.New("scram: empty password")
	}
	for i := 0; i < len(password); i++ {
		if c := password[i]; c < 0x21 || c > 0x7e {
			return Keys{}, errors.New("scram: the password must be printable ASCII without spaces")
		}
	}
	if len(salt) == 0 {
		return Keys{}, errors.New("scram: empty salt")
	}
	if iter < 1 {
		return Keys{}, errors.New("scram: iteration count must be positive")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		return Keys{}, err
	}
	ck := HMAC(salted, "Client Key")
	sk := sha256.Sum256(ck)
	return Keys{ClientKey: ck, StoredKey: sk[:], ServerKey: HMAC(salted, "Server Key")}, nil
}

// HMAC es HMAC-SHA-256(key, msg).
func HMAC(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// Verifier devuelve el verificador en el formato de Postgres.
func Verifier(password string, salt []byte, iter int) (string, error) {
	k, err := Derive(password, salt, iter)
	if err != nil {
		return "", err
	}
	b := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, b(salt), b(k.StoredKey), b(k.ServerKey)), nil
}

// NewVerifier es Verifier con una sal aleatoria nueva e Iterations.
func NewVerifier(password string) (string, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return Verifier(password, salt, Iterations)
}

// GeneratePassword devuelve PasswordBytes aleatorios en hexadecimal: sin nada
// que escapar en SQL, en una URL o en un fichero .pgpass.
func GeneratePassword() (string, error) {
	b := make([]byte, PasswordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
