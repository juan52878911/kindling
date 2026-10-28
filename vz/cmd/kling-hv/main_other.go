//go:build !darwin || !arm64

// En otros sistemas el binario existe solo para que `go build ./...` y `go vet`
// funcionen en cualquier máquina: kling-hv es Hypervisor.framework en Apple
// Silicon.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kling-hv: this helper only runs on macOS on Apple Silicon")
	os.Exit(1)
}
