package machine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// Una base cuyo /sbin/overlay-init es un enlace al agente (una imagen de
// Docker sin sh, con el init en Go) entiende las capas si el agente lleva la
// marca del init. debugfs cat no sigue enlaces: antes se leía el enlace, salía
// vacío y la base se rechazaba como anterior a las capas. Un agente sin la
// marca no vale aunque lleve la cadena kling.layer suelta, que la tienen
// también los agentes de antes del init en Go.
func TestBaseSupportsLayersSigueElEnlaceAlInitGo(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	src := func(contenido string) string {
		p, _ := fichero(t, contenido)
		return comillas(p)
	}
	elf := "\x7fELF" + strings.Repeat("\x00", 200<<10) // más que un trozo de lectura
	nuevo, viejo := elf+guest.InitMarker+"\x00", elf+api.LayerBootParam+"library\x00"
	base := func(nombre string, ordenes ...string) string {
		p := filepath.Join(dir, nombre+".ext4")
		crearExt4(t, p, append([]string{"mkdir sbin", "mkdir usr", "mkdir usr/local", "mkdir usr/local/bin"}, ordenes...)...)
		return p
	}
	casos := []struct {
		nombre  string
		ordenes []string
		quiero  bool
	}{
		{"script", []string{"write " + src("#!/bin/sh\ncase x in "+api.LayerBootParam+"=*) ;; esac\n") + " sbin/overlay-init"}, true},
		{"script-viejo", []string{"write " + src("#!/bin/sh\nexec /entrypoint\n") + " sbin/overlay-init"}, false},
		{"sin-init", nil, false},
		{"enlace-go", []string{"write " + src(nuevo) + " usr/local/bin/kling-guest",
			"symlink sbin/overlay-init /usr/local/bin/kling-guest"}, true},
		{"enlace-agente-viejo", []string{"write " + src(viejo) + " usr/local/bin/kling-guest",
			"symlink sbin/overlay-init /usr/local/bin/kling-guest"}, false},
		// Relativo y en cadena, como podría dejarlo otra herramienta.
		{"enlace-relativo", []string{"write " + src(nuevo) + " usr/local/bin/kling-guest",
			"symlink usr/local/bin/init ./kling-guest",
			"symlink sbin/overlay-init ../usr/local/bin/init"}, true},
		// El binario copiado en vez de enlazado también vale.
		{"copia-go", []string{"write " + src(nuevo) + " sbin/overlay-init"}, true},
	}
	ctx := context.Background()
	for _, c := range casos {
		ok, err := m.baseSupportsLayers(ctx, base(c.nombre, c.ordenes...))
		if err != nil || ok != c.quiero {
			t.Errorf("%s: %v, %v; quiero %v", c.nombre, ok, err, c.quiero)
		}
	}

	// Un bucle de enlaces es un error ("no lo sé"), no un "no" ni un cuelgue.
	bucle := base("bucle", "symlink sbin/overlay-init /sbin/b", "symlink sbin/b /sbin/overlay-init")
	if _, err := m.baseSupportsLayers(ctx, bucle); err == nil || !strings.Contains(err.Error(), "too many symlinks") {
		t.Errorf("bucle: %v", err)
	}
}
