package doctor

// Lo que otros comandos de kling db reutilizan de las reglas del doctor
// (kling db tenant-check explica con ellas por qué falla una política).

// FailOpenFix es el arreglo de una política fail-open (regla DB010).
const FailOpenFix = "make the policy fail closed: current_setting('<var>') without missing_ok (errors when unset), or compare only tenant_id = current_setting(...) with no IS NULL / '' / COALESCE escape"

// FailOpen dice si la expresión de una política deja pasar todas las filas
// con la variable de tenant nula o vacía, y por qué (la regla DB010).
func FailOpen(expr string) (why string, open bool) { return failOpen(expr) }

// TenantVars devuelve las variables que la expresión lee con current_setting.
func TenantVars(expr string) []string { return tenantVars(expr) }

// Safe deja un texto de la base listo para la terminal: sin caracteres de
// control y acotado a n runas.
func Safe(s string, n int) string { return safe(s, n) }
