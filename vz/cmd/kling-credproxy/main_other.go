//go:build !darwin

// En otros sistemas el binario existe solo para que `go build ./...` y `go vet`
// funcionen en cualquier máquina: en Linux el proxy es una goroutine del daemon.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kling-credproxy: this helper only runs on macOS; on Linux the credential proxy lives in the daemon")
	os.Exit(1)
}
