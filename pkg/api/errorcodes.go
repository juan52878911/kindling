package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Códigos de error estables (capacidad "error-codes").
//
// El cuerpo de error del daemon es {"message": "...", "code": "..."}: message
// es para personas y puede cambiar de redacción; code es para programas y no
// cambia. Antes solo había message, y IsMachineLimit o IsTenantQuota tenían que
// buscar una frase dentro del texto: el mensaje era contrato sin saberlo.
//
// Un código no se reutiliza ni se renombra (testdata/json-tags.txt los fija).
// Los específicos (machine_limit, name_taken…) van siempre con su código HTTP
// documentado; los genéricos salen del código HTTP (CodeForStatus) cuando el
// daemon no sabe nada más.

// CapabilityErrorCodes: el daemon pone code en el cuerpo de cada error JSON.
const CapabilityErrorCodes = "error-codes"

// ErrorCode es el código estable de un error del API.
type ErrorCode string

const (
	CodeBadRequest         ErrorCode = "bad_request"         // 400: la petición está mal
	CodeUnauthorized       ErrorCode = "unauthorized"        // 401: token inválido (authz)
	CodeForbidden          ErrorCode = "forbidden"           // 403: sin permiso (authz) o ruta reservada
	CodeNotFound           ErrorCode = "not_found"           // 404: no existe (o es ajeno, con authz)
	CodeConflict           ErrorCode = "conflict"            // 409: el estado no lo admite
	CodeNameTaken          ErrorCode = "name_taken"          // 409: el nombre de máquina ya está cogido
	CodeMachineLimit       ErrorCode = "machine_limit"       // 409: tope de máquinas del daemon
	CodeStaleGolden        ErrorCode = "stale_golden"        // 409: el dorado es de otro VMM o versión: rehacerlo
	CodeLengthRequired     ErrorCode = "length_required"     // 411
	CodePreconditionFailed ErrorCode = "precondition_failed" // 412: sha256 o versión esperados no casan
	CodeTooLarge           ErrorCode = "too_large"           // 413: cuerpo por encima del tope
	CodeUpgradeRequired    ErrorCode = "upgrade_required"    // 426
	CodeTenantQuota        ErrorCode = "tenant_quota"        // 429: cuota del inquilino agotada
	CodeTooManyRequests    ErrorCode = "too_many_requests"   // 429 que no es la cuota
	CodeInternal           ErrorCode = "internal"            // 500: fallo del host
	CodeUnsupported        ErrorCode = "unsupported"         // 501: este daemon o este VMM no lo hace
	CodeBadGateway         ErrorCode = "bad_gateway"         // 502: el invitado no contestó bien
	CodeUnavailable        ErrorCode = "unavailable"         // 503 que no es el disco
	CodeDiskFull           ErrorCode = "disk_full"           // 503: poco disco en el host
	CodeTimeout            ErrorCode = "timeout"             // 504
	CodeInsufficientMemory ErrorCode = "insufficient_memory" // 507: poca memoria en el host
)

// CodeForStatus es el código genérico de un código HTTP de error ("" si no
// es de error o no tiene uno propio).
func CodeForStatus(status int) ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return CodeBadRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusLengthRequired:
		return CodeLengthRequired
	case http.StatusPreconditionFailed:
		return CodePreconditionFailed
	case http.StatusRequestEntityTooLarge:
		return CodeTooLarge
	case http.StatusUpgradeRequired:
		return CodeUpgradeRequired
	case http.StatusTooManyRequests:
		return CodeTooManyRequests
	case http.StatusInternalServerError:
		return CodeInternal
	case http.StatusNotImplemented:
		return CodeUnsupported
	case http.StatusBadGateway:
		return CodeBadGateway
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	case http.StatusGatewayTimeout:
		return CodeTimeout
	case http.StatusInsufficientStorage:
		return CodeInsufficientMemory
	}
	return ""
}

// CodeOf es el código estable de un error del daemon, o "" si no es un
// *StatusError o el daemon es anterior a "error-codes".
func CodeOf(err error) ErrorCode {
	var se *StatusError
	if errors.As(err, &se) {
		return se.ErrorCode
	}
	return ""
}

// statusErrorDe arma el *StatusError de una respuesta de error con su cuerpo
// ya leído. Sin JSON o sin message, el texto es el estado HTTP.
func statusErrorDe(status int, statusText string, body []byte) *StatusError {
	var e Error
	if json.Unmarshal(body, &e) != nil || e.Message == "" {
		e.Message = statusText
	}
	return &StatusError{Code: status, Message: e.Message, ErrorCode: e.Code}
}

// esCodigo responde a las Is*: con code (daemon con "error-codes") manda el
// código; sin él, el estado HTTP y, si hace falta, la marca del texto, que es
// lo que había antes.
func esCodigo(err error, status int, code ErrorCode, marca string) bool {
	var se *StatusError
	if !errors.As(err, &se) || se.Code != status {
		return false
	}
	if se.ErrorCode != "" {
		return se.ErrorCode == code
	}
	return marca == "" || strings.Contains(se.Message, marca)
}

// IsNameTaken dice si un error es la negativa por un nombre de máquina que
// ya existe (POST /machines, 409).
func IsNameTaken(err error) bool {
	return esCodigo(err, http.StatusConflict, CodeNameTaken, nameTakenMark)
}

// nameTakenMark es el texto de ErrNameTaken del daemon, para daemons sin code.
const nameTakenMark = "machine name is taken"

// IsStaleGolden dice si un error es la negativa a instanciar un dorado
// obsoleto (de otro VMM o de una versión anterior): hay que rehacerlo.
func IsStaleGolden(err error) bool {
	return esCodigo(err, http.StatusConflict, CodeStaleGolden, staleGoldenMark)
}

// staleGoldenMark es el texto de ErrDoradoObsoleto del daemon.
const staleGoldenMark = "stale golden snapshot"
