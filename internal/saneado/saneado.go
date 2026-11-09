// Package saneado neutraliza los caracteres de control de un texto que viene
// del invitado (o de otra fuente que no controlamos) antes de que llegue a la
// terminal del operador: un ESC suelto basta para escribir en el portapapeles
// (OSC 52), cambiar el título, borrar la pantalla o falsear líneas anteriores.
//
// Cada carácter de control (C0, DEL y C1, que algunos terminales leen como
// ESC [) sale en su forma escapada visible (\x1b, \u009b) y los bytes que no
// son UTF-8 válido salen como \xNN. Las tres variantes difieren solo en qué
// espacios en blanco dejan pasar.
//
// No es para JSON: encoding/json ya escapa C0 y quien lee -json espera el
// texto tal cual. Ni para la salida de exec o shell, que el usuario pide cruda.
package saneado

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Consola deja \n, \r y \t: es la salida de una consola o un log, con sus
// retornos de carro.
func Consola(s string) string { return limpiar(s, "\n\r\t") }

// Texto deja \n y \t: un mensaje o un error de varias líneas. El \r se
// escapa porque volvería al principio de la línea y taparía lo anterior.
func Texto(s string) string { return limpiar(s, "\n\t") }

// Linea no deja nada: un campo que va en una sola fila (una columna de una
// tabla, un nombre) y que no debe partirla ni desalinearla.
func Linea(s string) string { return limpiar(s, "") }

func limpiar(s, deja string) string {
	limpio := true
	for _, r := range s {
		if r == utf8.RuneError || control(r, deja) {
			limpio = false
			break
		}
	}
	if limpio {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case control(r, deja):
			if r < 0x80 {
				fmt.Fprintf(&b, "\\x%02x", r)
			} else {
				fmt.Fprintf(&b, "\\u%04x", r)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

func control(r rune, deja string) bool {
	if r < 0x80 && strings.ContainsRune(deja, r) {
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
