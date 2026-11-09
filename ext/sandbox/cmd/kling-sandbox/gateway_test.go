package main

import "testing"

func TestLeerTenantsConAdmins(t *testing.T) {
	t.Setenv("KLING_SANDBOX_TOKEN", "")
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:10,ops:tok2")
	t.Setenv("KLING_SANDBOX_ADMINS", " ops ")
	ts, err := leerTenants()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Admin || !ts[1].Admin {
		t.Fatalf("tenants: %+v", ts)
	}

	// Un admin que no es ningún tenant es una errata, no un permiso que se ignora.
	t.Setenv("KLING_SANDBOX_ADMINS", "opss")
	if _, err := leerTenants(); err == nil {
		t.Fatal("expected an error for an unknown admin")
	}
}
