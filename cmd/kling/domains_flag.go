package main

import "strings"

// domainsFlag es -allow: se puede repetir (-allow a -allow b) o dar una lista
// separada por comas (-allow a,b), y las dos formas se suman. Antes era un
// String normal y un segundo -allow borraba el primero sin avisar: la máquina
// arrancaba sin salida a un dominio que el usuario sí había pedido.
type domainsFlag []string

func (d *domainsFlag) String() string { return strings.Join(*d, ",") }

func (d *domainsFlag) Set(v string) error {
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			*d = append(*d, x)
		}
	}
	return nil
}
