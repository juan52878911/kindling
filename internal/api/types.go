// Package api define el contrato entre el CLI y el daemon.
package api

import "time"

// State es el ciclo de vida de una microVM.
//
// StateWarm es lo que distingue a kindling de un runtime de contenedores: la
// máquina está congelada en un snapshot y despierta en decenas de milisegundos.
// No consume CPU ni RAM mientras está así, solo disco.
type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateWarm    State = "warm"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// Machine es una microVM gestionada por el daemon.
type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    State  `json:"state"`
	VCPUs    int    `json:"vcpus"`
	MemMiB   int    `json:"mem_mib"`
	PID      int    `json:"pid,omitempty"`
	LastErr  string `json:"last_error,omitempty"`
	SnapSize int64  `json:"snapshot_bytes,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	FrozenAt  *time.Time `json:"frozen_at,omitempty"`

	// Milisegundos de la última operación, para ver el coste real de cada fase.
	BootMS   int64 `json:"boot_ms,omitempty"`
	FreezeMS int64 `json:"freeze_ms,omitempty"`
	ThawMS   int64 `json:"thaw_ms,omitempty"`
}

// RunRequest crea y arranca una microVM.
type RunRequest struct {
	Name   string `json:"name,omitempty"`
	Image  string `json:"image,omitempty"`
	VCPUs  int    `json:"vcpus,omitempty"`
	MemMiB int    `json:"mem_mib,omitempty"`
}

// Event es un cambio de estado publicado en el bus del daemon.
type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	Message string    `json:"message,omitempty"`
}

// Tipos de evento.
const (
	EvCreated = "machine.created"
	EvStarted = "machine.started"
	EvFrozen  = "machine.frozen"
	EvThawed  = "machine.thawed"
	EvStopped = "machine.stopped"
	EvFailed  = "machine.failed"
)

// Info describe el daemon.
type Info struct {
	Version   string `json:"version"`
	Root      string `json:"root"`
	KVM       bool   `json:"kvm"`
	Machines  int    `json:"machines"`
	Firecrack string `json:"firecracker,omitempty"`
}

// Error es la respuesta de error de la API.
type Error struct {
	Message string `json:"message"`
}
