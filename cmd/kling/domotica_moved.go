package main

import "errors"

// Stub temporal: main.go todavía despacha el case "domotica". N7 lo quita al
// aplicar main_edits (aviso movedToExtension) y borra este fichero.
func cmdDomotica(args []string) error {
	return errors.New("domotica moved to an extension: kling plugins install domotica")
}
