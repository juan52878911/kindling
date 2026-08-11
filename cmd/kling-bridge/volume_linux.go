//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
)

// mountVolume monta el volumen si el kernel dice que hay uno.
//
// Devuelve el punto de montaje, o "" si no había volumen que montar — que es el
// caso normal y no un error.
func mountVolume() (mountpoint string, readOnly bool, err error) {
	mp, ro := volumeMountpoint()
	if mp == "" {
		return "", false, nil
	}
	if _, err := os.Stat(volumeDevice); err != nil {
		return "", false, fmt.Errorf("el kernel pide montar %s en %s pero %s no existe: %w",
			volumeBootParam, mp, volumeDevice, err)
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return "", false, fmt.Errorf("creando %s: %w", mp, err)
	}
	// data=ordered es el defecto de ext4 y aquí importa que lo sea: garantiza
	// que los datos llegan al disco ANTES que los metadatos que los referencian.
	// Sin eso, un corte a destiempo deja ficheros del tamaño correcto llenos de
	// basura, que es peor que no tenerlos.
	flags, opts := uintptr(0), "data=ordered"
	if ro {
		// noload además de MS_RDONLY: sin él, ext4 intentaría REPRODUCIR el
		// journal al montar, que es una escritura — y varias microVMs
		// reproduciéndolo a la vez sobre el mismo fichero es exactamente la
		// corrupción que el modo de solo lectura viene a evitar.
		flags, opts = syscall.MS_RDONLY, "noload"
	}
	if err := syscall.Mount(volumeDevice, mp, "ext4", flags, opts); err != nil {
		return "", false, fmt.Errorf("montando %s en %s: %w", volumeDevice, mp, err)
	}
	return mp, ro, nil
}

// volumeIsReadOnly dice si el volumen se montó en solo lectura. Un volumen así
// no hay que vaciarlo ni desmontarlo con cuidado: no hay nada que perder.
func volumeIsReadOnly() bool {
	_, ro := volumeMountpoint()
	return ro
}

// syncVolume vacía al disco lo que el invitado tenga en caché.
//
// Es lo ÚNICO que separa un volumen íntegro de uno corrupto: el daemon mata el
// VMM con SIGKILL, que para el invitado es un corte de corriente. Todo lo que
// siga en la caché de páginas en ese instante no llegó nunca al fichero del
// anfitrión. El daemon llama aquí antes de matar.
//
// Sync() no puede fallar ni bloquear indefinidamente sobre un virtio-blk local,
// así que no devuelve error: no hay nada que el que llama pudiera hacer.
func syncVolume(mp string) {
	if mp == "" || volumeIsReadOnly() {
		return
	}
	syscall.Sync()
}

// unmountVolume desmonta limpiamente. Deja el sistema de ficheros marcado como
// limpio, y así el siguiente arranque no tiene que reproducir el journal.
//
// Solo corre cuando el apagado es ordenado (SIGTERM). Si el daemon mata a lo
// bruto, esto no llega a ejecutarse — para eso está el journal, que convierte un
// corte en algo reproducible en vez de en corrupción.
func unmountVolume(mp string) {
	if mp == "" {
		return
	}
	if volumeIsReadOnly() {
		// Nada que vaciar: se desengancha y ya.
		_ = syscall.Unmount(mp, syscall.MNT_DETACH)
		return
	}
	syscall.Sync()
	// MNT_DETACH: si algún proceso del servidor MCP aún tiene el directorio
	// abierto, un desmontaje normal daría EBUSY y nos quedaríamos sin vaciar.
	// Con detach el árbol se desengancha y el sistema de ficheros se cierra en
	// cuanto se suelta la última referencia.
	if err := syscall.Unmount(mp, syscall.MNT_DETACH); err != nil {
		log.Printf("volumen: no pude desmontar %s: %v", mp, err)
		return
	}
	syscall.Sync()
	log.Printf("volumen desmontado limpiamente de %s", mp)
}
