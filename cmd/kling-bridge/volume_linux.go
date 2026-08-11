//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
)

// mountVolumes monta los volúmenes que pida el kernel, en orden.
//
// Devuelve nil si no había ninguno, que es el caso normal y no un error. Si uno
// falla, falla entero: montar la mitad dejaría al servidor MCP escribiendo en un
// directorio del overlay que desaparece con la máquina, y eso no da ningún error
// hasta que alguien busca lo que guardó.
func mountVolumes() ([]volumeSpec, error) {
	specs := volumeSpecs()
	if len(specs) == 0 {
		return nil, nil
	}
	for _, v := range specs {
		if _, err := os.Stat(v.device); err != nil {
			return nil, fmt.Errorf("el kernel pide montar %s en %s pero no existe: %w",
				v.device, v.mount, err)
		}
		if err := os.MkdirAll(v.mount, 0o755); err != nil {
			return nil, fmt.Errorf("creando %s: %w", v.mount, err)
		}
		// data=ordered es el defecto de ext4 y aquí importa que lo sea:
		// garantiza que los datos llegan al disco ANTES que los metadatos que
		// los referencian. Sin eso, un corte a destiempo deja ficheros del
		// tamaño correcto llenos de basura, que es peor que no tenerlos.
		flags, opts := uintptr(0), "data=ordered"
		if v.readOnly {
			// noload además de MS_RDONLY: sin él, ext4 intentaría REPRODUCIR el
			// journal al montar, que es una escritura — y varias microVMs
			// reproduciéndolo a la vez sobre el mismo fichero es exactamente la
			// corrupción que el modo de solo lectura viene a evitar.
			flags, opts = syscall.MS_RDONLY, "noload"
		}
		if err := syscall.Mount(v.device, v.mount, "ext4", flags, opts); err != nil {
			return nil, fmt.Errorf("montando %s en %s: %w", v.device, v.mount, err)
		}
		if v.readOnly {
			log.Printf("biblioteca compartida montada en %s (solo lectura)", v.mount)
		} else {
			log.Printf("volumen persistente montado en %s", v.mount)
		}
	}
	return specs, nil
}

// syncVolumes vacía al disco lo que el invitado tenga en caché.
//
// Es lo ÚNICO que separa un volumen íntegro de uno corrupto: el daemon mata el
// VMM con SIGKILL, que para el invitado es un corte de corriente. Todo lo que
// siga en la caché de páginas en ese instante no llegó nunca al fichero del
// anfitrión. El daemon llama aquí antes de matar.
//
// Sync() vacía TODOS los sistemas de ficheros de una vez, así que basta con
// saber que hay al menos uno escribible. No puede fallar ni bloquear
// indefinidamente sobre discos virtio locales, y por eso no devuelve error: no
// habría nada que hacer con él.
func syncVolumes(specs []volumeSpec) {
	for _, v := range specs {
		if !v.readOnly {
			syscall.Sync()
			return
		}
	}
}

// unmountVolumes desmonta limpiamente, dejando cada sistema de ficheros marcado
// como limpio para que el siguiente arranque no tenga que reproducir el journal.
//
// Solo corre cuando el apagado es ordenado (SIGTERM). Si el daemon mata a lo
// bruto, esto no llega a ejecutarse — para eso está el journal, que convierte un
// corte en algo reproducible en vez de en corrupción.
//
// En orden INVERSO al montaje: si un volumen se montó dentro de otro, el de
// dentro tiene que salir primero.
func unmountVolumes(specs []volumeSpec) {
	syncVolumes(specs)
	for i := len(specs) - 1; i >= 0; i-- {
		v := specs[i]
		// MNT_DETACH: si algún proceso del servidor MCP aún tiene el directorio
		// abierto, un desmontaje normal daría EBUSY y nos quedaríamos sin
		// vaciar. Con detach el árbol se desengancha y el sistema de ficheros se
		// cierra en cuanto se suelta la última referencia.
		if err := syscall.Unmount(v.mount, syscall.MNT_DETACH); err != nil {
			log.Printf("volumen: no pude desmontar %s: %v", v.mount, err)
			continue
		}
		if !v.readOnly {
			syscall.Sync()
		}
	}
}
