module github.com/juan52878911/kindling-mcp

go 1.24

require github.com/juan52878911/kindling v0.9.0

// Desarrollo: apunta al núcleo local con el backend nativo de macOS
// (api.Machine.Forwards/Addr, pkg/scheduler Addr) mientras v0.9.0 no está
// etiquetada en GitHub. Quitar en cuanto exista la etiqueta.
replace github.com/juan52878911/kindling => /Users/juanbedoya/Documents/GitHub/kindling/.claude/worktrees/plan-stage-2-impl-fa999b
