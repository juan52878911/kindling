package mcp

import "strings"

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
