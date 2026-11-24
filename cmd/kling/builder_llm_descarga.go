package main

// DESCARGA DE LO QUE VA A LA IMAGEN DE UN MODELO (constructor "llm").

// flujos es cuántas peticiones con rango van a la vez y tamTrozo cuánto pide
// cada una; variables para los tests.
var (
	flujos   = 8
	tamTrozo = int64(16 << 20)
)

// hashFichero calcula el sha256 de un fichero; variable para que los tests
// cuenten cuántas veces se relee entero.
var hashFichero = sha256File

// bootID es el identificador del arranque del núcleo; variable para los tests.
var bootID = func() string { return "" }
