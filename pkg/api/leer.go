package api

import (
	"errors"
	"fmt"
	"io"
)

// ErrBodyTooLarge es el error de LeerCuerpo cuando el cuerpo pasa del tope.
// Sirve igual para respuestas y para peticiones: quien sirve HTTP lo distingue
// con errors.Is y contesta 413 solo entonces; un fallo de lectura es otra cosa.
var ErrBodyTooLarge = errors.New("body too large")

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
		// Mensaje neutro: LeerCuerpo lee respuestas y también peticiones.
		return nil, fmt.Errorf("%w: it exceeds the %d-byte limit; "+
			"it was not truncated on purpose because a cut JSON is unreadable", ErrBodyTooLarge, max)
	}
	return b, nil
}
