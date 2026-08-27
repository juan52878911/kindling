package machine

// Leer las capacidades que una imagen declara, SIN montarla.
//
// La construcción (scripts/80-mcp-image.sh) inspecciona el árbol de dependencias
// npm y deja /etc/kling/capabilities.json dentro de la imagen: si usa navegador,
// si necesita salida a internet, y qué módulos nativos trae. El import lo lee
// para configurar el egress automáticamente —que el usuario no tenga que
// acordarse de `-egress internet`— y para avisar de módulos nativos que el
// empaquetado con --ignore-scripts pudo dejar sin su binario.
//
// Se lee con debugfs en frío, igual que imageHasBridge: montar en escritura solo
// para mirar un fichero es justo la operación que ya corrompió una imagen en
// este proyecto, y para esto ni hace falta.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// guestCapabilitiesPath es dónde deja el build las capacidades declaradas, visto
// desde dentro del invitado.
const guestCapabilitiesPath = "/etc/kling/capabilities.json"

// ImageCapabilities devuelve las capacidades declaradas por una imagen, o nil si
// no las declara (imágenes antiguas, o las que no son de npm): eso NO es un
// error, es una imagen que no trae la información.
func (m *Manager) ImageCapabilities(ctx context.Context, image string) (*api.Capabilities, error) {
	base, layer, err := m.imageLayer(image)
	if err != nil {
		return nil, err
	}
	bin := debugfsBin()
	if bin == "" {
		return nil, nil // sin debugfs no se puede saber; se falla abierto
	}
	// En una imagen por capas el fichero lo escribió el build, así que está en la
	// capa y no en la base compartida.
	path, inside := base, guestCapabilitiesPath
	if layer != "" {
		path, inside = layer, layerGuestPath(guestCapabilitiesPath)
	}
	out, err := exec.CommandContext(ctx, bin, "-R", "cat "+inside, path).Output()
	if err != nil {
		return nil, nil // el fichero no existe: imagen sin capacidades declaradas
	}
	// debugfs puede anteponer texto; nos quedamos desde la primera llave.
	s := strings.TrimSpace(string(out))
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return nil, nil
	}
	var c api.Capabilities
	if json.Unmarshal([]byte(s[i:]), &c) != nil {
		return nil, nil
	}
	return &c, nil
}

// debugfsBin localiza debugfs, que en Debian vive en /sbin y no siempre está en
// el PATH de un servicio de systemd.
func debugfsBin() string {
	if bin, err := exec.LookPath("debugfs"); err == nil {
		return bin
	}
	for _, p := range []string{"/sbin/debugfs", "/usr/sbin/debugfs"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
