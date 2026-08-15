package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// layerFixture deja un directorio de imágenes con los ficheros pedidos.
func layerFixture(t *testing.T, files map[string]string) *Manager {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, "images", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{root: root}
}

// Una imagen monolítica sigue siendo un solo disco. Es el caso de todo lo que ya
// está construido: si esto se rompe, se rompe el parque entero.
func TestImageLayerMonolithic(t *testing.T) {
	m := layerFixture(t, map[string]string{"files.ext4": "x"})
	base, layer, err := m.imageLayer("files")
	if err != nil {
		t.Fatal(err)
	}
	if base != m.imagePath("files") {
		t.Fatalf("base = %q, want %q", base, m.imagePath("files"))
	}
	if layer != "" {
		t.Fatalf("una imagen monolítica no tiene capa, y salió %q", layer)
	}
}

// Con las dos presentes manda la monolítica: arrancar por el camino nuevo
// cambiaría lo que ve un servicio que hoy funciona.
func TestImageLayerMonolithicWins(t *testing.T) {
	m := layerFixture(t, map[string]string{
		"files.ext4":       "x",
		"files.layer.ext4": "y",
		"min.ext4":         "b",
	})
	_, layer, err := m.imageLayer("files")
	if err != nil {
		t.Fatal(err)
	}
	if layer != "" {
		t.Fatalf("con .ext4 presente no se debe usar la capa, y salió %q", layer)
	}
}

// La base sale de la receta, que es lo único que sabe sobre qué se construyó.
func TestImageLayerResolvesBaseFromRecipe(t *testing.T) {
	m := layerFixture(t, map[string]string{
		"files.layer.ext4":   "y",
		"files.recipe.json":  `{"name":"files","base":"node"}`,
		"node.ext4":          "b",
		"min.ext4":           "otra",
		"overlay-template.e": "",
	})
	base, layer, err := m.imageLayer("files")
	if err != nil {
		t.Fatal(err)
	}
	if base != m.imagePath("node") {
		t.Fatalf("base = %q, want la de la receta (node)", base)
	}
	if layer != m.layerPath("files") {
		t.Fatalf("layer = %q, want %q", layer, m.layerPath("files"))
	}
}

// Sin receta —o con Base vacío, que es lo que deja el build por defecto— la base
// es la que asume 80-mcp-image.sh.
func TestImageLayerDefaultBase(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"sin receta": {"files.layer.ext4": "y", "min.ext4": "b"},
		"base vacía": {"files.layer.ext4": "y", "min.ext4": "b",
			"files.recipe.json": `{"name":"files"}`},
		"receta rota": {"files.layer.ext4": "y", "min.ext4": "b",
			"files.recipe.json": `{ esto no es json`},
	} {
		t.Run(name, func(t *testing.T) {
			m := layerFixture(t, files)
			base, _, err := m.imageLayer("files")
			if err != nil {
				t.Fatal(err)
			}
			if base != m.imagePath(defaultBaseImage) {
				t.Fatalf("base = %q, want %q", base, m.imagePath(defaultBaseImage))
			}
		})
	}
}

// Una capa cuya base no está en disco no se puede arrancar, y el error tiene que
// decir QUÉ falta: sin eso el fallo aparece dentro del invitado.
func TestImageLayerOrphan(t *testing.T) {
	m := layerFixture(t, map[string]string{"files.layer.ext4": "y"})
	if _, _, err := m.imageLayer("files"); err == nil {
		t.Fatal("una capa sin base debe fallar")
	} else if !strings.Contains(err.Error(), defaultBaseImage) {
		t.Fatalf("el error no nombra la base que falta: %v", err)
	}
}

func TestImageLayerMissing(t *testing.T) {
	m := layerFixture(t, nil)
	if _, _, err := m.imageLayer("files"); err == nil {
		t.Fatal("una imagen que no existe debe fallar")
	}
}

// El device de la capa depende del número de volúmenes porque va DETRÁS de
// ellos: colarla antes correría las letras y el puente monta cada volumen por
// posición desde vdc.
func TestLayerDevice(t *testing.T) {
	for nvols, want := range map[int]string{0: "/dev/vdc", 1: "/dev/vdd", 4: "/dev/vdg"} {
		got, err := layerDevice(nvols)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("con %d volúmenes: %q, want %q", nvols, got, want)
		}
	}
	if _, err := layerDevice(30); err == nil {
		t.Fatal("pasadas las letras de disco hay que fallar, no dar un device inventado")
	}
}

// La línea de comandos de una imagen monolítica no cambia ni un byte: es la
// misma con la que arrancan todos los snapshots ya congelados.
func TestBootArgsLayer(t *testing.T) {
	legacy := bootArgs(nil, false, "")
	if strings.Contains(legacy, api.LayerBootParam) {
		t.Fatalf("sin capa no debe aparecer %s: %q", api.LayerBootParam, legacy)
	}
	con := bootArgs([]api.VolumeAttachment{{Mount: "/data"}}, false, "/dev/vdd")
	if !strings.Contains(con, api.LayerBootParam+"=/dev/vdd") {
		t.Fatalf("falta la capa en %q", con)
	}
	// Y el volumen sigue ahí: los dos parámetros conviven.
	if !strings.Contains(con, api.VolumeBootParam+"=/data") {
		t.Fatalf("la capa se comió el volumen: %q", con)
	}
}

// El prefijo /upper no es decorativo: es dónde deja el delta el build. Leerlo
// sin él daría "no lleva puente" en una imagen que sí lo lleva.
func TestLayerGuestPath(t *testing.T) {
	if got, want := layerGuestPath(guestBridgePath), "/upper/"+guestBridgePath; got != want {
		t.Fatalf("layerGuestPath = %q, want %q", got, want)
	}
	// Y no debe salir con doble barra: la ruta del invitado ya viene absoluta.
	if got := layerGuestPath(guestCapabilitiesPath); strings.Contains(got, "//") {
		t.Fatalf("ruta con doble barra: %q", got)
	}
}

// Images() lista una imagen por capas UNA vez y con su nombre, no como
// "files.layer": el sufijo largo también acaba en .ext4.
func TestImagesListaCapas(t *testing.T) {
	m := layerFixture(t, map[string]string{
		"min.ext4":              "b",
		"files.layer.ext4":      "y",
		"legacy.ext4":           "x",
		"overlay-template.ext4": "molde",
		"vmlinux":               "kernel",
	})
	got := m.Images()
	want := []string{"files", "legacy", "min"}
	if len(got) != len(want) {
		t.Fatalf("Images() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Images() = %v, want %v", got, want)
		}
	}
}

// Una máquina por capas retiene su capa Y la base: reescribir la base mientras
// corre le corrompería el sistema de ficheros por debajo.
func TestImageUsersRetieneLaBase(t *testing.T) {
	m := layerFixture(t, map[string]string{
		"min.ext4":          "b",
		"files.layer.ext4":  "y",
		"files.recipe.json": `{"name":"files","base":"min"}`,
	})
	m.byID = map[string]*api.Machine{
		"a": {ID: "a", Name: "files-1", Image: "files", State: api.StateRunning},
		// Una parada no retiene nada: ya no lee de la imagen.
		"b": {ID: "b", Name: "files-2", Image: "files", State: api.StateStopped},
	}
	users := m.imageUsers()
	if len(users["files"]) != 1 {
		t.Errorf("users[files] = %v, want 1", users["files"])
	}
	if len(users["min"]) != 1 {
		t.Errorf("users[min] = %v: la base tiene que quedar retenida por la capa viva", users["min"])
	}
}

// ImageFile es lo que se mide y lo que se enseña como ruta: de una imagen por
// capas, su capa.
func TestImageFile(t *testing.T) {
	m := layerFixture(t, map[string]string{
		"min.ext4":         "b",
		"files.layer.ext4": "y",
		"legacy.ext4":      "x",
	})
	if got := m.ImageFile("files"); got != m.layerPath("files") {
		t.Errorf("ImageFile(files) = %q, want la capa", got)
	}
	if got := m.ImageFile("legacy"); got != m.imagePath("legacy") {
		t.Errorf("ImageFile(legacy) = %q, want el ext4 monolítico", got)
	}
}

// Los dos extremos del contrato están en lenguajes distintos: el anfitrión
// escribe kling.layer= en Go y el invitado lo lee en sh. Renombrar la constante
// no toca los scripts, y el fallo se vería como un invitado sin /entrypoint —un
// pánico del kernel—, no como un test en rojo. Este test es ese test.
func TestInitScriptsReadLayerParam(t *testing.T) {
	// minimal-init es el que 70-build-minimal-image.sh instala como
	// /sbin/overlay-init en la base 'min', que es la de los servicios MCP;
	// overlay-init es el de la base con systemd. Los dos tienen que entenderlo.
	for _, script := range []string{"minimal-init.sh", "overlay-init.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "scripts", script))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, api.LayerBootParam+"=") {
			t.Errorf("%s no lee %s=", script, api.LayerBootParam)
		}
		if !strings.Contains(s, "/"+layerUpperDir+":/") {
			t.Errorf("%s no pone la capa de lower con el prefijo /%s", script, layerUpperDir)
		}
	}
}
