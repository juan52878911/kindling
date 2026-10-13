// Package mysqlpw calcula en el host lo que MySQL y MariaDB guardan de una
// contraseña, para que la contraseña de una copia de kling db no entre nunca
// en el invitado: al invitado va solo el hash, por stdin (ALTER USER ...
// IDENTIFIED WITH mysql_native_password AS '<hash>').
//
// POR QUÉ mysql_native_password: es el único método que MariaDB (el motor de
// las plantillas de Alpine) y MySQL 8.0 entienden igual y cuyo formato
// guardado se calcula sin más: "*" + HEX(SHA1(SHA1(clave))), sin sal. Su
// debilidad conocida es esa (SHA-1 rápido y sin sal: un hash filtrado se ataca
// por diccionario), y con una clave de 192 bits aleatorios (la que genera
// kling db) no hay diccionario que valga. caching_sha2_password (el de MySQL
// 8.4+ por defecto, y el único de MySQL 9) está en sha2.go, validado contra
// un MySQL 8.4 real: las plantillas de MySQL de Oracle lo usan.
package mysqlpw

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// NativePlugin es el nombre del plugin cuyo hash da NativeHash.
const NativePlugin = "mysql_native_password"

// reHash es la forma de un hash de mysql_native_password.
var reHash = regexp.MustCompile(`^\*[0-9A-F]{40}$`)

// NativeHash es el authentication_string de mysql_native_password para pw:
// "*" y 40 dígitos hexadecimales en mayúsculas.
func NativeHash(pw string) string {
	s1 := sha1.Sum([]byte(pw))
	s2 := sha1.Sum(s1[:])
	return "*" + strings.ToUpper(hex.EncodeToString(s2[:]))
}

// ValidHash dice si h tiene la forma de un hash de mysql_native_password
// (y por tanto se puede meter entre comillas simples en SQL sin escapar).
func ValidHash(h string) bool { return reHash.MatchString(h) }

// Hash es el authentication_string de pw para el método plugin: el que ya
// tiene el usuario, para que rotar no le cambie de método (MySQL 8.4 trae
// mysql_native_password apagado y MySQL 9 no lo trae).
func Hash(plugin, pw string) (string, error) {
	switch plugin {
	case NativePlugin:
		return NativeHash(pw), nil
	case CachingSHA2Plugin:
		return NewCachingSHA2Hash(pw)
	}
	return "", fmt.Errorf("kling db can't compute a %q password hash (only %s and %s)", plugin, NativePlugin, CachingSHA2Plugin)
}

// Check dice si auth es el hash de pw con el método plugin.
func Check(plugin, pw, auth string) bool {
	switch plugin {
	case NativePlugin:
		return ValidHash(auth) && NativeHash(pw) == auth
	case CachingSHA2Plugin:
		return CheckCachingSHA2(pw, auth)
	}
	return false
}

// ValidHashFor dice si h tiene la forma de un hash que kling db sabe comprobar
// con el método plugin (y meter en SQL sin escapar).
func ValidHashFor(plugin, h string) bool {
	switch plugin {
	case NativePlugin:
		return ValidHash(h)
	case CachingSHA2Plugin:
		return ValidCachingSHA2Hash(h)
	}
	return false
}
