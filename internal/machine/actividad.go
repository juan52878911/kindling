package machine

import "time"

// Actividad es el uso de la máquina id que ve el daemon fuera de sus puertos
// publicados (internal/publicar, daemon.idle_freeze): peticiones en curso
// contra ella (Usar: exec, shell, ficheros, logs que se siguen...), cuándo
// acabó la última, y sesiones vivas del broker de aristas (macOS) que salen
// de ella o van a ella. En Linux las aristas van por los proxies de
// internal/net y no se cuentan aquí.
func (m *Manager) Actividad(id string) (enCurso int, ultima time.Time, sesiones int) {
	m.bat.mu.Lock()
	if u := m.bat.uso[id]; u != nil {
		enCurso, ultima = u.enCurso, u.ultimo
	}
	m.bat.mu.Unlock()
	return enCurso, ultima, sesionesBroker.de(id)
}

// de cuenta las sesiones vivas desde o hacia la máquina id.
func (r *registroBroker) de(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for s := range r.sesiones {
		s.mu.Lock()
		if !s.cortada && (s.origen == id || s.maquina == id) {
			n++
		}
		s.mu.Unlock()
	}
	return n
}
