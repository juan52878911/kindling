package main

import (
	"errors"
	"strings"
	"testing"
)

func dnsFalso(cuerpo string, err error) posterCrudo {
	return func(metodo, ruta, c string) ([]byte, error) { return []byte(cuerpo), err }
}

// La configuracion se juzga SIN tocar la red. Es la mitad que caza el fallo real
// —el resolv.conf del anfitrion viajando dentro— de forma determinista.
func TestComprobarDNSCazaLaConfiguracionInservible(t *testing.T) {
	casos := []struct {
		nombre  string
		cuerpo  string
		falla   bool
		mencion string
	}{
		{
			"127.0.0.53: lo que deja systemd-resolved, y dentro no existe",
			`{"nameservers":["127.0.0.53"],"resuelve":false,"error":"i/o timeout"}`,
			true, "unreachable",
		},
		{
			"la IP privada del router, que el cortafuegos bloquea a proposito",
			`{"nameservers":["192.168.2.1"],"resuelve":false,"error":"i/o timeout"}`,
			true, "unreachable",
		},
		{
			"sin ninguna linea nameserver",
			`{"nameservers":[],"error":"/etc/resolv.conf has no nameserver line"}`,
			true, "NO nameserver",
		},
		{
			"resolver publico y resolviendo: todo bien",
			`{"nameservers":["1.1.1.1","8.8.8.8"],"resuelve":true,"probado":"example.com"}`,
			false, "",
		},
		{
			// La distincion que importa: la configuracion esta bien y aun asi no
			// resuelve. Es un problema, pero de otra clase, y el mensaje lo dice.
			"configuracion buena pero sin salida",
			`{"nameservers":["1.1.1.1"],"resuelve":false,"probado":"example.com","error":"no route to host"}`,
			true, "look usable",
		},
		{
			"uno inservible y otro bueno: basta con uno",
			`{"nameservers":["127.0.0.53","1.1.1.1"],"resuelve":true}`,
			false, "",
		},
	}
	for _, c := range casos {
		err := comprobarDNS(dnsFalso(c.cuerpo, nil))
		if (err != nil) != c.falla {
			t.Errorf("%s: err=%v, esperaba fallo=%v", c.nombre, err, c.falla)
			continue
		}
		if c.falla && !strings.Contains(err.Error(), c.mencion) {
			t.Errorf("%s: el error no menciona %q: %v", c.nombre, c.mencion, err)
		}
	}
}

// Un puente antiguo no tiene /dns. Eso NO es un fallo del servicio, y tratarlo
// como tal marcaria enfermo a todo lo que no se haya reconstruido aun.
func TestUnPuenteSinEndpointDeDNSNoEsUnFallo(t *testing.T) {
	if err := comprobarDNS(dnsFalso("", errors.New("HTTP 404 from /dns"))); err != nil {
		t.Errorf("un puente sin /dns se reporto como fallo: %v", err)
	}
	if err := comprobarDNS(dnsFalso("no soy json", nil)); err != nil {
		t.Errorf("una respuesta ilegible se reporto como fallo: %v", err)
	}
}
