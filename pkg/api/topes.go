package api

import "fmt"

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

// ValidateAllowDomains comprueba cuántos dominios trae allow_domains (que
// cada uno sea un dominio lo mira quien monta la red).
func ValidateAllowDomains(domains []string) error {
	if len(domains) > MaxAllowDomains {
		return fmt.Errorf("allow_domains has %d entries; the limit is %d", len(domains), MaxAllowDomains)
	}
	return nil
}
