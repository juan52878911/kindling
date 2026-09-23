package api

import (
	"fmt"
	"io"
)

// LeerCuerpo lee como mucho `max` bytes y FALLA si habia mas.
//
// El patron habitual, `io.ReadAll(io.LimitReader(r, max))`, trunca EN SILENCIO:
// devuelve exactamente max bytes con err == nil, y quien lo parsea recibe un
// JSON cortado a mitad. El error que se ve entonces habla de sintaxis —"invalid
// character" en una posicion cualquiera— y desde ahi no hay forma de llegar a la
// causa, que es que la respuesta no cabia.
//
// Pasa de verdad en este sistema: una captura de pantalla de un servicio de
// navegador viaja en base64 dentro del JSON-RPC, y un PDF o una pagina larga
// pasan de los topes con facilidad.
//
// Se lee max+1 para poder DISTINGUIR "justo el tope" de "se paso", sin cargar
// en memoria lo que sobra.
func LeerCuerpo(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("the response exceeds the %d-byte limit; "+
			"it was not truncated on purpose because a cut JSON is unreadable", max)
	}
	return b, nil
}
