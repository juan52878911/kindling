package telemetria

import (
	"net/http"
	"strings"
)

// Ruta es lo que se sabe de una petición mirando solo método y camino.
type Ruta struct {
	// Plantilla es la ruta sin ids ("/v1/sandboxes/{id}/exec"): lo que va a
	// http.route, para que la cardinalidad no crezca con cada sandbox.
	Plantilla string
	// Sandbox es el id "host/máquina", vacío si la ruta no es de un sandbox.
	Sandbox string
	// Accion es el tipo de acción con efecto ("exec", "file.write"...); vacío
	// para lecturas y rutas sin efecto, que no se auditan.
	Accion string
	// Param lleva ids de la ruta: "tenant", "key", "pid", "session", "volume", "graph".
	Param map[string]string
	// Cuerpo pide que el envoltorio guarde el principio del cuerpo de la
	// petición (los que la auditoría necesita: argv, puerto...).
	Cuerpo bool
	// Respuesta pide guardar el principio de la respuesta (el id del sandbox creado).
	Respuesta bool
}

// Clasificar traduce método y camino a una Ruta. Lo desconocido da la
// plantilla "other" (nunca el camino crudo, que puede llevar ids o ser basura).
func Clasificar(metodo, camino string) Ruta {
	r := Ruta{Plantilla: "other"}
	switch {
	case camino == "/v1/health":
		r.Plantilla = camino
	case camino == "/v1/metrics" || camino == "/v1/hosts" || camino == "/v1/templates" || camino == "/v1/audit":
		r.Plantilla = camino
	case camino == "/v1/sandboxes":
		r.Plantilla = camino
		if metodo == http.MethodPost {
			r.Accion, r.Cuerpo, r.Respuesta = "sandbox.create", true, true
		}
	case strings.HasPrefix(camino, "/v1/sandboxes/"):
		clasificarSandbox(&r, metodo, strings.TrimPrefix(camino, "/v1/sandboxes/"))
	case camino == "/v1/volumes":
		r.Plantilla = camino
		if metodo == http.MethodPost {
			r.Accion = "volume.create"
		}
	case strings.HasPrefix(camino, "/v1/volumes/"):
		r.Plantilla = "/v1/volumes/{name}"
		r.Param = map[string]string{"volume": strings.TrimPrefix(camino, "/v1/volumes/")}
		if metodo == http.MethodDelete {
			r.Accion = "volume.delete"
		}
	case camino == "/v1/graphs":
		r.Plantilla = camino
		if metodo == http.MethodPost {
			r.Accion = "graph.claim"
		}
	case strings.HasPrefix(camino, "/v1/graphs/"):
		r.Plantilla = "/v1/graphs/{id}"
		r.Param = map[string]string{"graph": strings.TrimPrefix(camino, "/v1/graphs/")}
		if metodo == http.MethodDelete {
			r.Accion = "graph.release"
		}
	case camino == "/v1/tenants":
		r.Plantilla = camino
		if metodo == http.MethodPost {
			r.Accion = "tenant.create"
		}
	case strings.HasPrefix(camino, "/v1/tenants/"):
		clasificarTenant(&r, metodo, strings.Split(strings.TrimPrefix(camino, "/v1/tenants/"), "/"))
	}
	return r
}

func clasificarTenant(r *Ruta, metodo string, p []string) {
	r.Param = map[string]string{"tenant": p[0]}
	switch {
	case len(p) == 1:
		r.Plantilla = "/v1/tenants/{id}"
		switch metodo {
		case http.MethodPatch:
			r.Accion = "tenant.update"
		case http.MethodDelete:
			r.Accion = "tenant.delete"
		}
	case len(p) == 2 && p[1] == "keys":
		r.Plantilla = "/v1/tenants/{id}/keys"
		if metodo == http.MethodPost {
			r.Accion = "key.create"
		}
	case len(p) == 3 && p[1] == "keys":
		r.Plantilla = "/v1/tenants/{id}/keys/{kid}"
		r.Param["key"] = p[2]
		if metodo == http.MethodDelete {
			r.Accion = "key.revoke"
		}
	case len(p) == 4 && p[1] == "keys" && p[3] == "rotate":
		r.Plantilla = "/v1/tenants/{id}/keys/{kid}/rotate"
		r.Param["key"] = p[2]
		if metodo == http.MethodPost {
			r.Accion = "key.rotate"
		}
	}
}

func clasificarSandbox(r *Ruta, metodo, resto string) {
	p := strings.Split(resto, "/")
	if len(p) < 2 || p[0] == "" || p[1] == "" {
		return
	}
	r.Sandbox = p[0] + "/" + p[1]
	r.Plantilla = "/v1/sandboxes/{id}"
	sub := p[2:]
	if len(sub) == 0 {
		if metodo == http.MethodDelete {
			r.Accion = "sandbox.delete"
		}
		return
	}
	r.Plantilla += "/" + sub[0]
	post := metodo == http.MethodPost
	switch sub[0] {
	case "renew":
		if post && len(sub) == 1 {
			r.Accion = "sandbox.renew"
		}
	case "exec":
		if post && len(sub) == 1 {
			r.Accion, r.Cuerpo = "exec", true
		}
	case "shell":
		if post && len(sub) == 1 {
			r.Accion = "shell.open"
		}
	case "endpoints":
		if post && len(sub) == 1 {
			r.Accion, r.Cuerpo = "endpoint.create", true
		}
	case "files":
		if len(sub) == 1 {
			switch metodo {
			case http.MethodPut:
				r.Accion = "file.write"
			case http.MethodDelete:
				r.Accion = "file.delete"
			}
		}
	case "processes":
		clasificarProcesos(r, metodo, sub[1:])
	case "sessions":
		switch {
		case len(sub) == 1 && post:
			r.Accion, r.Cuerpo = "session.create", true
		case len(sub) == 2 && sub[1] != "":
			r.Plantilla += "/{sid}"
			r.Param = map[string]string{"session": sub[1]}
			if metodo == http.MethodDelete {
				r.Accion = "session.delete"
			}
		}
	}
}

func clasificarProcesos(r *Ruta, metodo string, p []string) {
	post := metodo == http.MethodPost
	switch len(p) {
	case 0:
		if post {
			r.Accion, r.Cuerpo = "process.start", true
		}
	case 1, 2:
		r.Plantilla += "/{pid}"
		r.Param = map[string]string{"pid": p[0]}
		switch {
		case len(p) == 1 && metodo == http.MethodDelete:
			r.Accion = "process.kill"
		case len(p) == 2:
			r.Plantilla += "/" + p[1]
			if p[1] == "signal" && post {
				r.Accion, r.Cuerpo = "process.signal", true
			}
		}
	}
}
