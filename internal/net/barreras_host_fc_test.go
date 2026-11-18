//go:build !darwin

package net

func listadosDelNetns() [][]string {
	return [][]string{argsIptablesNetns("kl-x", "-S", "FORWARD")}
}
