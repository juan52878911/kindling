package api

// GuestProcsPath (GET) son los procesos de usuario vivos en el invitado, sin el
// agente: lo que pregunta `kling save` antes de volcar un dorado. Cada uno que
// esté vivo en el volcado despierta igual en todas las copias, con los mismos
// aleatorios en memoria (issue #175). Ruta de control: solo lee /proc.
const GuestProcsPath = "/procs"

// GuestProcsMaxNames acota GuestProcs.Names.
const GuestProcsMaxNames = 8

// GuestProcs es la respuesta de GET /procs.
type GuestProcs struct {
	// Count son los procesos de usuario vivos: ni hilos del kernel, ni el
	// agente ni sus antecesores, ni zombis.
	Count int `json:"count"`
	// Names son sus nombres (el comm de /proc), sin repetir, ordenados y como
	// mucho GuestProcsMaxNames.
	Names []string `json:"names,omitempty"`
	// Covered son los de Count que pertenecen al servicio de la imagen cuando
	// este declara OnRestore: cada copia los reinicia o les manda su señal.
	Covered int `json:"covered,omitempty"`
}
