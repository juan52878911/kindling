//go:build !darwin

// En otros sistemas el binario existe solo para que `go build ./...` y `go vet`
// funcionen en cualquier máquina: en Linux el núcleo usa Firecracker.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kling-vz: this helper only runs on macOS (Apple Silicon); on Linux kindling uses Firecracker")
	os.Exit(1)
}
