package api

import "strings"

// EsFalloTSC reconoce el unico fallo de restauracion que se cura reconstruyendo
// el dorado, y no hay que confundir con "el servicio esta roto".
//
// Un snapshot de Firecracker graba la frecuencia del TSC del anfitrion, que se
// mide de nuevo en cada arranque. Al reiniciar el host, TODOS los dorados dejan
// de restaurar a la vez. No es corrupcion: los ficheros estan intactos y el
// servicio es correcto; simplemente ya no valen en esta maquina.
//
// Reconoce las dos formas del mismo fallo: el error crudo de Firecracker
// ("Could not set TSC scaling ... Invalid argument") y la traduccion que
// explainRestoreErr le pone encima, que es la que cruza el API HTTP y la unica
// que ve el CLI. Ambas conservan el token, y por eso se busca ese y no la frase
// entera: la frase la escribimos nosotros y cambiaria al editar un comentario.
func EsFalloTSC(err error) bool {
	return err != nil && strings.Contains(err.Error(), "TSC")
}
