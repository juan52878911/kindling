// Package scripts lleva dentro del binario los scripts del núcleo que un
// constructor en Go mete tal cual en una imagen (el init mínimo), para que
// construir no dependa de que estén instalados en /usr/local/lib/kindling.
package scripts

import _ "embed"

// MinimalInit es minimal-init.sh: el /sbin/overlay-init de las bases.
//
//go:embed minimal-init.sh
var MinimalInit string
