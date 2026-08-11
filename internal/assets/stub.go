//go:build !linux || !embed_assets

// El caso NORMAL: el binario no lleva artefactos.
//
// Es el que compila en un Mac, en el CI y en cualquier clon recién hecho del
// repo, y por eso es el que no puede depender de que exista ningún fichero.

package assets

import "io/fs"

func source() (fs.FS, bool) { return nil, false }
