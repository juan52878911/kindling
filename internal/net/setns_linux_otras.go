//go:build linux && !amd64 && !arm64

package net

// Sin el número de setns en esta arquitectura: anadirIPSet cae al comando.
// Variable y no constante: uintptr(-1) constante no compila.
var sysSetns = -1
