package main

// Hacer que los paquetes de un volumen se ENCUENTREN solos.
//
// Montar una biblioteca compartida en /libs no basta: node y python solo miran
// donde se les dice. Sin esto, los paquetes están ahí y el servidor MCP no los
// ve, que es la forma más cara de tener una biblioteca — parece un problema del
// servidor y es de configuración.
//
// Se calcula AQUÍ y no en el entrypoint de la imagen a propósito. El punto de
// montaje se decide al arrancar la microVM y viaja en la línea de comandos del
// kernel, precisamente para que la misma imagen sirva con la biblioteca montada
// donde sea. Un NODE_PATH incrustado al construir la imagen mentiría en cuanto
// alguien la montase en otro sitio, y mentiría en silencio.

import (
	"os"
	"path/filepath"
	"strings"
)

// libraryEnv devuelve el entorno del servidor MCP con NODE_PATH y PYTHONPATH
// apuntando a los volúmenes montados que de verdad contienen paquetes.
//
// Se COMPRUEBA que el directorio existe en vez de añadirlo a ciegas: una entrada
// inventada en NODE_PATH hace que node recorra rutas inexistentes en cada
// require, y una en PYTHONPATH aparece en los rastros de pila de cualquier
// import fallido, mandando a quien depura al sitio equivocado.
//
// Las rutas del volumen van DESPUÉS de lo que ya hubiera: lo que la imagen trae
// instalado manda sobre la biblioteca compartida. Al revés, actualizar el
// volumen cambiaría en silencio la versión que usa un servicio que ya funcionaba.
func libraryEnv(base []string, vols []volumeSpec) []string {
	var node, py []string
	for _, v := range vols {
		if v.mount == "" {
			continue
		}
		// npm install --prefix <vol> deja los paquetes en <vol>/node_modules.
		if dir := filepath.Join(v.mount, "node_modules"); isDir(dir) {
			node = append(node, dir)
		}
		// pip tiene dos formas y las dos son comunes:
		//   pip install --target <vol>   -> los paquetes CUELGAN del volumen
		//   pip install --prefix <vol>   -> <vol>/lib/pythonX.Y/site-packages
		if hasPythonPackages(v.mount) {
			py = append(py, v.mount)
		}
		if dirs, _ := filepath.Glob(filepath.Join(v.mount, "lib", "python*", "site-packages")); len(dirs) > 0 {
			for _, d := range dirs {
				if isDir(d) {
					py = append(py, d)
				}
			}
		}
	}
	env := base
	env = appendPath(env, "NODE_PATH", node)
	env = appendPath(env, "PYTHONPATH", py)
	return env
}

// appendPath añade rutas a una variable de tipo PATH conservando la que ya
// hubiera. Devuelve el entorno tal cual si no hay nada que añadir: exportar una
// variable vacía no es lo mismo que no exportarla, y algunas herramientas tratan
// un PYTHONPATH="" como una entrada de ruta vacía, que significa "el directorio
// actual".
func appendPath(env []string, key string, dirs []string) []string {
	if len(dirs) == 0 {
		return env
	}
	prefijo := key + "="
	out := make([]string, 0, len(env)+1)
	nuevo := strings.Join(dirs, string(os.PathListSeparator))
	visto := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefijo); ok {
			visto = true
			if v != "" {
				kv = prefijo + v + string(os.PathListSeparator) + nuevo
			} else {
				kv = prefijo + nuevo
			}
		}
		out = append(out, kv)
	}
	if !visto {
		out = append(out, prefijo+nuevo)
	}
	return out
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// hasPythonPackages reconoce el reparto de `pip install --target`, donde los
// paquetes cuelgan directamente del directorio.
//
// Se busca la marca de metadatos (.dist-info / .egg-info) y no cualquier
// directorio, porque un volumen de datos cualquiera tiene carpetas y añadirlo a
// PYTHONPATH pondría su contenido en el camino de búsqueda de imports — un
// fichero llamado json.py ahí dentro tapa el módulo json de la biblioteca
// estándar, y el fallo aparece lejísimos de su causa.
func hasPythonPackages(dir string) bool {
	for _, patron := range []string{"*.dist-info", "*.egg-info"} {
		if m, _ := filepath.Glob(filepath.Join(dir, patron)); len(m) > 0 {
			return true
		}
	}
	return false
}
