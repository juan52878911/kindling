package api

import (
	"fmt"
	"net/netip"
	"strings"
	"unicode"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// Topes de entrada de run, sandbox, fork y PUT labels. Sin ellos, cada
// máquina guardaba en memoria y en machines.json lo que quisiera quien la
// pidió (el cuerpo solo está acotado a 1 MiB).
const (
	// MaxLabels y MaxLabelsBytes acotan las etiquetas de una máquina:
	// cuántas y cuánto suman claves y valores.
	MaxLabels      = 64
	MaxLabelsBytes = 4 << 10
	// MaxAllowDomains es el tope de allow_domains con egress "allowlist".
	MaxAllowDomains = 256
)

// ValidateLabels comprueba solo el tamaño de un juego de etiquetas; las
// reservadas las rechaza ValidateNoGraphLabels. nil y vacío valen.
func ValidateLabels(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("too many labels (%d); the limit is %d", len(labels), MaxLabels)
	}
	total := 0
	for k, v := range labels {
		total += len(k) + len(v)
	}
	if total > MaxLabelsBytes {
		return fmt.Errorf("labels add up to %d bytes; the limit is %d", total, MaxLabelsBytes)
	}
	return nil
}

// ValidateAllowDomains comprueba allow_domains: cuántos trae y que cada uno sea
// un nombre de dominio que el resolver del allowlist sepa casar. Antes solo se
// contaban, y lo que no casaba nunca fallaba cerrado y en silencio: el
// invitado recibía REFUSED sin que nada dijera por qué. Así pasaba con
// "*.a.com" (el resolver compara igual o sufijo, y "a.com" ya cubre sus
// subdominios), con un nombre con acentos sin pasar a punycode y con una IP
// (el allowlist abre lo que resuelve el DNS, no direcciones). Las vacías se
// ignoran, como siempre.
func ValidateAllowDomains(domains []string) error {
	if len(domains) > MaxAllowDomains {
		return fmt.Errorf("allow_domains has %d entries; the limit is %d", len(domains), MaxAllowDomains)
	}
	for _, d := range domains {
		if err := validarDominio(d); err != nil {
			return fmt.Errorf("allow_domains: %w", err)
		}
	}
	return nil
}

// reDominio es un nombre DNS en ASCII: etiquetas de letras, dígitos, '-' y
// '_' (como _dmarc), de hasta 63 bytes y sin '-' en los extremos.
var reDominio = lazyre.New(`^([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)(\.[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)*$`)

// validarDominio comprueba una entrada de allow_domains con la misma
// normalización que el resolver (minúsculas, sin el punto final).
func validarDominio(d string) error {
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	switch {
	case n == "":
		return nil
	case strings.Contains(n, "*"):
		base := strings.TrimPrefix(strings.TrimLeft(n, "*"), ".")
		if base == "" || strings.Contains(base, "*") {
			return fmt.Errorf("%q: wildcards are not supported; a domain already allows all its subdomains", d)
		}
		return fmt.Errorf("%q: wildcards are not supported; write %q, which already allows all its subdomains", d, base)
	case esIP(n):
		return fmt.Errorf("%q: allow_domains takes domain names, not IP addresses", d)
	}
	for _, r := range n {
		if r > unicode.MaxASCII {
			return fmt.Errorf("%q: write internationalized names in punycode (xn--...)", d)
		}
	}
	if len(n) > 253 || !reDominio.MatchString(n) {
		return fmt.Errorf("%q is not a domain name", d)
	}
	return nil
}

// esIP dice si n es una dirección IP (la v6, con o sin corchetes).
func esIP(n string) bool {
	_, err := netip.ParseAddr(strings.Trim(n, "[]"))
	return err == nil
}
