package dbstate

import (
	"crypto/sha256"
	"encoding/hex"
)

// S3AccessKey es el identificador de acceso (GK + 24 hexadecimales, la forma
// que exige Garage) de una copia S3 con ese secreto. Sale del secreto, así que
// el host solo guarda un fichero y cada copia, al rotar el secreto, cambia
// también de identificador: la clave del dorado no vale en ninguna copia.
func S3AccessKey(secret string) string {
	h := sha256.Sum256([]byte("kling-db-s3-access\n" + secret))
	return "GK" + hex.EncodeToString(h[:])[:24]
}
