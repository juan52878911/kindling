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

// MotivoImagenCambiada es lo que se graba en la salud de un servicio cuando su
// imagen se actualizo POR DEBAJO del dorado.
//
// `kling images refresh` mete el puente nuevo en la imagen, pero el snapshot
// dorado se congelo con el viejo dentro y con esas paginas mapeadas. A partir de
// ahi el servicio despierta y NO sirve — visto en el laboratorio: "tool did not
// start listening", que no menciona la imagen por ningun sitio.
//
// El comando avisaba de que habia que reimportar, y un aviso impreso no impide
// nada: basta no leerlo. Grabarlo en la salud lo convierte en algo que la sonda
// ve y que `mcp heal` sabe curar.
const MotivoImagenCambiada = "its image was refreshed under the golden snapshot"

// EsImagenCambiada reconoce esa causa. Como la del TSC, es curable
// reconstruyendo — y por la misma razon: los ficheros estan bien, lo que ya no
// vale es el snapshot.
func EsImagenCambiada(err error) bool {
	return err != nil && strings.Contains(err.Error(), MotivoImagenCambiada)
}
