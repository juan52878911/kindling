package hosts

import (
	"encoding/base32"
	"errors"
	"strings"
)

// Los ids del gateway son "host/máquina": con barra, que no cabe en un nombre
// DNS. IDaDNS los codifica en base32 minúsculas sin relleno (solo a-z y 2-7,
// que un nombre de host admite y que no distingue mayúsculas, como DNS), e
// IDdeDNS lo deshace. Es la codificación de la entrada HTTP a puertos
// (<puerto>-<id>.<dominio>) y la que otro paquete que necesite poner un id del
// gateway en un nombre DNS puede reutilizar.

// MaxEtiquetaDNS es el máximo de una etiqueta DNS (RFC 1035).
const MaxEtiquetaDNS = 63

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDaDNS codifica un id del gateway para usarlo en una etiqueta DNS.
func IDaDNS(id string) string { return strings.ToLower(b32.EncodeToString([]byte(id))) }

// IDdeDNS deshace IDaDNS.
func IDdeDNS(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty id")
	}
	b, err := b32.DecodeString(strings.ToUpper(s))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
