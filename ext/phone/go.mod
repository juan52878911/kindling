module github.com/juan52878911/kindling/ext/phone

go 1.24

require github.com/juan52878911/kindling v0.17.0

// El núcleo vive dos directorios arriba en el mismo repo: el replace relativo
// está siempre para que GOWORK=off y quien clone sin workspace compilen contra
// el núcleo del mismo commit, nunca contra una etiqueta bajada.
replace github.com/juan52878911/kindling => ../..
