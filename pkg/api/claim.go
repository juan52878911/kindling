package api

// Reclamo atómico de un sandbox precalentado: POST /sandboxes/claim.
//
// El gateway mantiene un pool de sandboxes running y sin dueño. Reclamar uno
// era una danza de dos fases con una espera fija (el daemon no tenía
// compare-and-swap de etiquetas); con esta llamada el daemon elige, etiqueta y
// renueva en una sola sección crítica.

import (
	"context"
	"net/http"
)

// CapabilityClaim: el daemon sirve POST /sandboxes/claim.
const CapabilityClaim = "sandbox-claim"

// LabelClaim es la etiqueta con la que un frontal marca lo que ha reclamado.
// El daemon nunca reclama una máquina que ya la lleva ni una con dueño
// (LabelOwner): lo que ya es de alguien no se roba.
const LabelClaim = "kling.sandbox.claim"

// CodeNoClaimable acompaña al 404 de un reclamo sin candidata.
const CodeNoClaimable ErrorCode = "no_claimable"

// ClaimRequest pide reclamar UNA máquina running con AllowExec, sin
// transición en curso y sin dueño ni reclamo, cuyas etiquetas incluyan Match
// y no incluyan ninguna clave de Absent.
type ClaimRequest struct {
	// Match: etiquetas que la candidata debe llevar (clave y valor). No puede
	// ir vacío: reclamar "cualquiera" no es una petición válida.
	Match map[string]string `json:"match"`
	// Absent: claves que la candidata no puede llevar (se suman a las que el
	// daemon exige siempre: dueño, reclamo y las del fondo del daemon).
	Absent []string `json:"absent,omitempty"`
	// Labels se suman a las de la máquina elegida. Mismas reglas que
	// PUT /machines/{ref}/labels. Con LabelOwner se comprueba la cuota del dueño.
	Labels map[string]string `json:"labels,omitempty"`
	// Name renombra la máquina; vacío conserva el suyo.
	Name string `json:"name,omitempty"`
	// TTLSeconds y OnTTL: como en RunRequest; el reloj arranca al reclamar.
	// 0 / vacío conservan los de la máquina.
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	OnTTL      string `json:"on_ttl,omitempty"`
}

// ClaimSandbox reclama una máquina (POST /sandboxes/claim). Sin candidata, el
// error cumple IsNoClaimable.
func (c *Client) ClaimSandbox(ctx context.Context, req ClaimRequest) (*Machine, error) {
	var m Machine
	if err := c.do(ctx, http.MethodPost, "/sandboxes/claim", req, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// IsNoClaimable dice si un error es «no hay ninguna máquina que reclamar».
func IsNoClaimable(err error) bool {
	return esCodigo(err, http.StatusNotFound, CodeNoClaimable, "no claimable")
}
