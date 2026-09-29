module github.com/juan52878911/kindling/ext/db

// La misma versión de Go que el núcleo y el resto de ext/ (lo comprueba CI):
// es el mínimo que se promete. El go.work pide la 1.26.3 solo por vz/.
go 1.24

require github.com/juan52878911/kindling v0.17.0

// El núcleo vive dos directorios arriba en el mismo repo: el replace relativo
// está siempre para que GOWORK=off y quien clone sin workspace compilen contra
// el núcleo del mismo commit, nunca contra una etiqueta bajada.
replace github.com/juan52878911/kindling => ../..
