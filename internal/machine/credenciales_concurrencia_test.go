package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Varias llamadas a la vez a SetSnapshotCredentials sobre la misma plantilla,
// cada una con su variable: al final están TODAS. Sin cerrojo, cada una leía
// el almacén de antes y la última en escribir borraba lo de las demás; y con
// el temporal de nombre fijo, dos escrituras se pisaban el fichero.
func TestCredencialesDePlantillaConcurrentesNoSePierden(t *testing.T) {
	m := newTestManager(t)
	escribirSnapshot(t, m, "svc-al", api.Snapshot{Egress: "allowlist", AllowDomains: []string{"example.org"}})

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.SetSnapshotCredentials("svc-al", []api.CredentialSpec{{
				Domain: fmt.Sprintf("api%d.example.com", i), Env: fmt.Sprintf("KEY_%02d", i), Secret: credSecreto,
			}}, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("SetSnapshotCredentials: %v", err)
		}
	}

	specs, err := m.cargarCredencialesPlantilla("svc-al")
	if err != nil {
		t.Fatal(err)
	}
	var envs []string
	for _, s := range specs {
		envs = append(envs, s.Env)
	}
	sort.Strings(envs)
	if len(envs) != n {
		t.Fatalf("quedaron %d de %d credenciales: %v", len(envs), n, envs)
	}

	// Y ningún temporal a medias junto al almacén.
	es, _ := os.ReadDir(filepath.Dir(m.credSnapPath("svc-al")))
	for _, e := range es {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("quedó un temporal: %s", e.Name())
		}
	}
}
