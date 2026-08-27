package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// La autocuracion rehace el servicio COMO ESTABA. Si perdiera un campo por el
// camino, la caida se "arreglaria" cambiando el servicio por debajo: un stateful
// convertido en efimero pierde lo que acumula en cada llamada, y un egress
// acotado que vuelve a "internet" abre la LAN. Ninguna de las dos cosas daria
// error — por eso hay que comprobarlas aqui.
func TestArgvReimportarConservaLaConfiguracionDelDorado(t *testing.T) {
	s := &api.Snapshot{
		Name:         "semgrep",
		Image:        "semgrep",
		MemMiB:       1024,
		VCPUs:        2,
		CPUPct:       200,
		Egress:       "allowlist",
		AllowDomains: []string{"semgrep.dev", "registry.npmjs.org"},
		Volumes: []api.VolumeAttachment{
			{Name: "codigo", Mount: "/data", ReadOnly: true},
			{Name: "cache", Mount: "/cache"},
		},
		Labels: map[string]string{api.LabelService: "semgrep", api.LabelStateful: "true"},
	}

	argv := argvReimportar(s, "")

	valor := func(bandera string) string {
		i := slices.Index(argv, bandera)
		if i < 0 || i+1 >= len(argv) {
			t.Fatalf("falta la bandera %s en %v", bandera, argv)
		}
		return argv[i+1]
	}

	for _, c := range []struct{ bandera, quiero string }{
		{"-image", "semgrep"},
		{"-mem", "1024"},
		{"-cpus", "2"},
		{"-cpu-pct", "200"},
		{"-egress", "allowlist"},
		{"-allow", "semgrep.dev,registry.npmjs.org"},
	} {
		if got := valor(c.bandera); got != c.quiero {
			t.Errorf("%s = %q, esperaba %q", c.bandera, got, c.quiero)
		}
	}

	if !slices.Contains(argv, "-force") {
		t.Error("sin -force el import se negaria: el dorado ya existe")
	}
	if !slices.Contains(argv, "-stateful") || slices.Contains(argv, "-ephemeral") {
		t.Errorf("un servicio stateful debe reimportarse stateful: %v", argv)
	}
	if argv[len(argv)-1] != "semgrep" {
		t.Errorf("el nombre del servicio debe ir de posicional al final: %v", argv)
	}
}

func TestArgvReimportarNoInventaBanderasQueElDoradoNoTiene(t *testing.T) {
	s := &api.Snapshot{
		Name:   "memory",
		Image:  "memory",
		Labels: map[string]string{api.LabelService: "memory"},
	}
	argv := argvReimportar(s, "")

	// Un cero no es "por defecto": es "no lo grabo". Pasar -mem 0 fijaria cero.
	for _, no := range []string{"-mem", "-cpus", "-cpu-pct", "-egress", "-allow", "-volume"} {
		if slices.Contains(argv, no) {
			t.Errorf("%s no deberia aparecer si el dorado no lo grabo: %v", no, argv)
		}
	}
	if !slices.Contains(argv, "-ephemeral") {
		t.Errorf("sin etiqueta stateful, el servicio es efimero: %v", argv)
	}
}

// El generador y el parser de -volume tienen que entenderse. Aqui se comprueba
// contra volumeFlag.Set, que es el parser DE VERDAD, no una copia.
func TestElVolumenGeneradoLoEntiendeElParserReal(t *testing.T) {
	casos := []api.VolumeAttachment{
		{Name: "datos", Mount: "/data"},
		{Name: "datos", Mount: "/data", ReadOnly: true},
		{Name: "suelto"},
		{Name: "ro-sin-punto", ReadOnly: true},
	}

	for _, quiero := range casos {
		var f volumeFlag
		if err := f.Set(specVolumen(quiero)); err != nil {
			t.Errorf("%+v -> %q: el parser lo rechaza: %v", quiero, specVolumen(quiero), err)
			continue
		}
		if len(f) != 1 {
			t.Errorf("%q dio %d volumenes", specVolumen(quiero), len(f))
			continue
		}
		got := f[0]
		got.DriveID = "" // lo asigna el VMM, no el argv
		if !reflect.DeepEqual(got, quiero) {
			t.Errorf("ida y vuelta por %q: %+v, esperaba %+v", specVolumen(quiero), got, quiero)
		}
	}
}

func TestArgvReimportarPropagaElHost(t *testing.T) {
	s := &api.Snapshot{Name: "x", Labels: map[string]string{api.LabelService: "x"}}
	if slices.Contains(argvReimportar(s, ""), "-H") {
		t.Error("sin -H, no debe inventarse uno")
	}
	argv := argvReimportar(s, "ssh://juan@lab")
	i := slices.Index(argv, "-H")
	if i < 0 || argv[i+1] != "ssh://juan@lab" {
		t.Errorf("el -H del vigia debe llegar al import: %v", argv)
	}
}
