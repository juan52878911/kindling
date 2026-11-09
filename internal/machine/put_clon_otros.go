//go:build !darwin

package machine

// clonImagenPut: fuera de macOS, intentarPut monta la imagen y no usa debugfs
// salvo sin mount; sin clonefile, se edita en su sitio.
func clonImagenPut(string) string { return "" }
