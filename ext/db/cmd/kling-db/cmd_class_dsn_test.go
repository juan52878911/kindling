package main

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// class -passwords y connect -dsn dan el mismo esquema por motor: una copia
// Redis no puede salir con una DSN postgres://.
func TestEngineDSN(t *testing.T) {
	for _, c := range []struct{ engine, want string }{
		{enginePostgres, "postgres://app:pw@127.0.0.1:5432/appdb?sslmode=disable"},
		{engineMySQL, "mysql://app:pw@127.0.0.1:5432/appdb"},
		{engineRedis, "redis://app:pw@127.0.0.1:5432/0"},
		{engineFerret, "mongodb://app:pw@127.0.0.1:5432/appdb?authMechanism=SCRAM-SHA-256"},
	} {
		if got := engineDSN(c.engine, "app", "pw", "127.0.0.1", 5432, "appdb"); got != c.want {
			t.Errorf("%s: %s, quería %s", c.engine, got, c.want)
		}
	}
}

// Una copia SQLite lista cuenta como lista aunque no tenga clave en el host.
func TestClassSQLiteListaSinClave(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	mc := &api.Machine{ID: "0123456789abcdef", Name: "sqc-01", Labels: map[string]string{
		labelEngine: engineSQLite, labelState: stateReady, labelGolden: "sq",
		"kling.db.role": "app", "kling.db.database": "appdb"}}
	r := classRowOf(mc)
	if !r.Ready || r.PasswordFile != "" {
		t.Fatalf("SQLite: ready=%v fichero=%q", r.Ready, r.PasswordFile)
	}
	mc.Labels[labelEngine] = engineRedis
	if classRowOf(mc).Ready {
		t.Fatal("una copia Redis sin clave en el host no está lista")
	}
}

// FerretDB es un motor conocido, con el puerto de Mongo, sin cliente propio
// (se conecta con el DSN) y con la contraseña de un rol de Postgres.
func TestEngineFerret(t *testing.T) {
	l := map[string]string{labelEngine: engineFerret}
	if engineOf(l) != engineFerret || !knownEngine(engineFerret) || !pgBacked(engineFerret) || !hasPassword(engineFerret) {
		t.Fatal("ferretdb no se reconoce como motor con Postgres por debajo")
	}
	if enginePort(engineFerret) != 27017 || clientMode(engineFerret) != "dsn" || engineDoc(engineFerret) != "docs/db-engines.md" {
		t.Fatalf("puerto %d, cliente %q, doc %q", enginePort(engineFerret), clientMode(engineFerret), engineDoc(engineFerret))
	}
	if pgBacked(engineMySQL) || pgBacked(engineRedis) {
		t.Fatal("pgBacked de un motor que no es Postgres")
	}
}
