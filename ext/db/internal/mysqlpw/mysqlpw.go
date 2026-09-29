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
// 8.4+ por defecto) guarda un SHA-256-crypt con sal y 5000 rondas; calcularlo
// aquí es posible pero no se ha validado contra un servidor real, y queda
// pendiente (docs/mysql.md).
package mysqlpw

import (
	"crypto/sha1"
	"encoding/hex"
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
