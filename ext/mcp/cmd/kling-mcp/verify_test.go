package main

import "testing"

// La consola real de semgrep: intenta pip install y falla sin red.
const semgrepLog = `
[    0.9] Linux version 6.1.0
2026/08/12 21:19:50 sesión bb0b984a: servidor MCP arrancado (pid 328)
MCP Server Semgrep running on stdio (allowed roots: /)
[   74.08] pysemgrep invoked oom-killer
Error setting up Semgrep: Error installing Semgrep: Command failed: pip3 install semgrep
error: externally-managed-environment
`

// Una consola sana: el servidor arranca y responde, sin instalar nada.
const cleanLog = `
[    0.9] Linux version 6.1.0
2026/08/12 21:25:20 sesión a1b2c3d4: servidor MCP arrancado (pid 301)
Context7 MCP server running on stdio
2026/08/12 21:25:20 tools/list servido: 2 herramientas
`

func TestDetectRuntimeInstall_Semgrep(t *testing.T) {
	hits := detectRuntimeInstall(semgrepLog)
	if len(hits) == 0 {
		t.Fatal("no detectó la instalación en caliente de semgrep")
	}
	// Debe reconocer tanto el comando pip como el rechazo del sistema.
	var pip, pep bool
	for _, h := range hits {
		if contains(h, "pip3 install") || contains(h, "pip") {
			pip = true
		}
		if contains(h, "PEP 668") {
			pep = true
		}
	}
	if !pip || !pep {
		t.Fatalf("evidencia incompleta: pip=%v pep=%v · %v", pip, pep, hits)
	}
}

func TestDetectRuntimeInstall_Limpio(t *testing.T) {
	if hits := detectRuntimeInstall(cleanLog); len(hits) != 0 {
		t.Fatalf("falso positivo en consola sana: %v", hits)
	}
}

func TestDetectRuntimeInstall_Dedup(t *testing.T) {
	// La misma huella repetida cien veces cuenta una sola vez.
	log := ""
	for i := 0; i < 100; i++ {
		log += "npm install left-pad\n"
	}
	if hits := detectRuntimeInstall(log); len(hits) != 1 {
		t.Fatalf("esperaba 1 evidencia deduplicada, hubo %d", len(hits))
	}
}

func TestDetectRuntimeInstall_Npx(t *testing.T) {
	hits := detectRuntimeInstall("running npx -y @scope/some-mcp-server\nENOTFOUND registry.npmjs.org")
	if len(hits) < 2 {
		t.Fatalf("esperaba detectar npx y el fallo de registro, hubo %v", hits)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
