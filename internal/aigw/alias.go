package aigw

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// UN DORADO POR INQUILINO (pendiente 4 de la ronda 8).
//
// chispa-server elige sus bancos por el texto de la pregunta, sin mirar quién
// pregunta: dos inquilinos en el mismo proceso los comparten. Mientras eso no
// cambie, el aislamiento es el proceso, es decir, un dorado (con sus bancos)
// por inquilino. Cada inquilino tiene su bloque, con tenants de uno, y alias
// da a todos la misma URL: POST /v1/m/<alias>/… llega al bloque del inquilino
// del token.
//
// Por defecto cerrado: con inquilinos declarados, un chispa-decide con dorado
// sirve a exactamente uno salvo shared: true, que queda escrito en el fichero.

// errAliasPrincipal: el token principal no es de ningún inquilino, así que un
// alias no dice a qué bloque ir.
var errAliasPrincipal = errors.New("alias needs a tenant token")

// validateAliases comprueba lo que depende de varios bloques: alias, shared y
// la regla de un inquilino de chispa-decide. tenants son los declarados.
func (c *Config) validateAliases(tenants map[string]bool) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	porAlias := map[string]map[string]string{} // alias → inquilino → modelo
	type dueño struct{ modelo, tenants string }
	porDorado := map[string]dueño{}
	for _, n := range sortedKeys(c.Models) {
		m := c.Models[n]
		if m == nil || m.Kind != KindHTTP {
			continue // alias y shared fuera de kind http ya los rechaza validateBackend
		}
		decide := m.Contract == ContractChispaDecide
		if m.Shared && !decide {
			add("model %q: shared is for contract chispa-decide (tenants sharing one golden's banks)", n)
		}
		if decide && m.Snapshot != "" && len(tenants) > 0 && len(m.Tenants) != 1 && !m.Shared {
			add("model %q: with tenants declared, a chispa-decide golden serves exactly one (its banks are in the golden): give it \"tenants\": [one], one block per tenant (alias keeps one URL), or set \"shared\": true to share its banks on purpose", n)
		}
		if a := m.Alias; a != "" {
			switch {
			case !nameRE.MatchString(a):
				add("model %q: alias %q is not a valid name (lowercase letters, digits, . _ -)", n, a)
			case c.Models[a] != nil:
				add("model %q: alias %q is the name of a model", n, a)
			case m.Contract != ContractChispaDecide && m.Contract != ContractRaw:
				add("model %q: alias is for contracts served under /v1/m (chispa-decide, raw)", n)
			case len(m.Tenants) == 0:
				add("model %q: a block with alias %q needs the tenants it serves", n, a)
			}
			if porAlias[a] == nil {
				porAlias[a] = map[string]string{}
			}
			for _, tn := range m.Tenants {
				if otro, ok := porAlias[a][tn]; ok {
					add("model %q: tenant %q already has a block under alias %q (%q)", n, tn, a, otro)
				}
				porAlias[a][tn] = n
			}
		}
		// Un dorado lleva los bancos de quien lo fabricó: dos bloques con
		// inquilinos distintos no lo pueden compartir.
		if m.Snapshot != "" && (decide || m.Alias != "") {
			ts := slices.Clone(m.Tenants)
			slices.Sort(ts)
			clave := strings.Join(ts, ",")
			if d, ok := porDorado[m.Snapshot]; ok && d.tenants != clave {
				add("models %q and %q share snapshot %q with different tenants: each tenant needs its own golden", d.modelo, n, m.Snapshot)
			} else if !ok {
				porDorado[m.Snapshot] = dueño{modelo: n, tenants: clave}
			}
		}
	}
	return errs
}

// indexarAlias añade a c el índice por alias de sus backends.
func (c *catalogo) indexarAlias() {
	for _, b := range c.lista {
		if b.Alias == "" {
			continue
		}
		if c.byAlias == nil {
			c.byAlias = map[string][]*Backend{}
		}
		c.byAlias[b.Alias] = append(c.byAlias[b.Alias], b)
	}
}

// resolver da el backend de /v1/m/{nombre}: el de ese nombre si existe; si no,
// el bloque del alias nombre cuyo tenants contiene a inquilino. nil sin
// coincidencia (el llamante da el mismo 404 que a un modelo que no existe,
// para no revelar que el alias lo usan otros); errAliasPrincipal con el token
// principal, junto con los nombres concretos.
func (c *catalogo) resolver(nombre, inquilino string) (*Backend, []string, error) {
	if b := c.byName[nombre]; b != nil {
		return b, nil, nil
	}
	bs := c.byAlias[nombre]
	if len(bs) == 0 {
		return nil, nil, nil
	}
	if inquilino == "default" {
		nombres := make([]string, len(bs))
		for i, b := range bs {
			nombres[i] = b.Name
		}
		return nil, nombres, errAliasPrincipal
	}
	for _, b := range bs {
		if slices.Contains(b.Tenants, inquilino) {
			return b, nil, nil
		}
	}
	return nil, nil, nil
}
