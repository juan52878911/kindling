package gateway

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// REPARACIÓN DE TIPOS.
//
// kindling preserva los tipos JSON de extremo a extremo, pero varios clientes y
// modelos los estropean antes de que lleguen aquí. Los daños típicos:
//
//	array    -> objeto con claves "0", "1", "2"...
//	array    -> string con el JSON dentro:  "[{\"a\":1}]"
//	number   -> string:  "42"
//	boolean  -> string:  "true"
//
// El servidor MCP los rechaza con "expected array, received object", y desde
// fuera parece un fallo de la herramienta cuando en realidad nunca llegó a verla.
//
// Como el catálogo guarda el esquema declarado de cada herramienta, se puede
// deshacer el destrozo: se compara el valor recibido con el tipo que la
// herramienta dice esperar y se convierte cuando la intención es inequívoca.
//
// Solo se toca lo que está claramente mal. Un valor que ya cuadra con su esquema
// se deja intacto: reparar de más sería peor que no reparar.

// coerceArgs ajusta los argumentos al esquema declarado por la herramienta.
// Si algo no encaja o el esquema falta, devuelve la entrada sin tocar.
func coerceArgs(args, schema json.RawMessage) json.RawMessage {
	if len(args) == 0 || len(schema) == 0 {
		return args
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return args
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return args
	}

	fixed := coerce(v, s)
	out, err := json.Marshal(fixed)
	if err != nil {
		return args
	}
	return out
}

func coerce(v any, schema map[string]any) any {
	if schema == nil {
		return v
	}
	switch schemaType(schema) {
	case "object":
		return coerceObject(v, schema)
	case "array":
		return coerceArray(v, schema)
	case "number", "integer":
		return coerceNumber(v)
	case "boolean":
		return coerceBool(v)
	case "string":
		return v // convertir a string casi siempre es lo que ya se pretendía
	default:
		return v
	}
}

// schemaType extrae el tipo, tolerando la forma ["string","null"] de algunos
// esquemas, donde el primero no nulo es el que manda.
func schemaType(schema map[string]any) string {
	switch t := schema["type"].(type) {
	case string:
		return t
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

func coerceObject(v any, schema map[string]any) any {
	m, ok := v.(map[string]any)
	if !ok {
		// Un objeto que llegó como string con JSON dentro.
		if s, ok := v.(string); ok {
			var parsed map[string]any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				m = parsed
			}
		}
		if m == nil {
			return v
		}
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return m
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if ps, ok := props[k].(map[string]any); ok {
			out[k] = coerce(val, ps)
			continue
		}
		out[k] = val
	}
	return out
}

func coerceArray(v any, schema map[string]any) any {
	items, _ := schema["items"].(map[string]any)

	fixItems := func(xs []any) []any {
		if items == nil {
			return xs
		}
		for i := range xs {
			xs[i] = coerce(xs[i], items)
		}
		return xs
	}

	switch x := v.(type) {
	case []any:
		return fixItems(x)

	case string:
		// El array viajó serializado dentro de un string.
		var parsed []any
		if json.Unmarshal([]byte(x), &parsed) == nil {
			return fixItems(parsed)
		}
		return v

	case map[string]any:
		// El daño más común: el array se convirtió en objeto indexado por
		// posición. Solo se deshace si TODAS las claves son numéricas; si no,
		// era un objeto de verdad y no hay nada que reparar.
		keys := make([]int, 0, len(x))
		for k := range x {
			n, err := strconv.Atoi(k)
			if err != nil {
				return v
			}
			keys = append(keys, n)
		}
		sort.Ints(keys)
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, x[strconv.Itoa(k)])
		}
		return fixItems(out)
	}
	return v
}

func coerceNumber(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return n
	}
	return v
}

func coerceBool(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return true
	case "false":
		return false
	}
	return v
}
