package machine

import "sync"

// cerrojos es un registro de mutex POR MAQUINA con contador de referencias.
//
// El registro anterior era un sync.Map de id -> *sync.Mutex, y tenia un fallo
// que ninguna cantidad de cuidado en el orden de las llamadas arregla: al
// retirar la entrada de una maquina que se borra, quien ya estuviera ESPERANDO
// ese mutex se queda con un puntero perfectamente valido, y el siguiente en
// llamar crea otro con LoadOrStore. Dos goroutines, dos mutex distintos, la
// misma maquina: la exclusion desaparece justo cuando mas hace falta, que es
// mientras se borra.
//
// Mover el Delete al final del Remove acortaba la ventana; no la cerraba.
//
// Con contador de referencias la entrada vive mientras alguien la tenga O la
// espere, y se retira sola cuando sale el ultimo. No hay que borrarla a mano
// desde ningun sitio —esa era la fuente del problema— y el mapa no crece con
// cada maquina que pasa por aqui.
type cerrojos struct {
	mu sync.Mutex
	m  map[string]*cerrojo
}

type cerrojo struct {
	mu sync.Mutex
	// refs cuenta a los que lo tienen y a los que lo esperan. Se toca SOLO bajo
	// cerrojos.mu, nunca bajo cerrojo.mu: un contador protegido por el mismo
	// mutex que cuenta no serviría de nada.
	refs int
}

func nuevosCerrojos() *cerrojos {
	return &cerrojos{m: map[string]*cerrojo{}}
}

// tomar bloquea hasta conseguir el cerrojo de esa maquina y devuelve la funcion
// que lo suelta. La funcion es idempotente en el sentido que importa: soltarla
// dos veces seria un error de programacion, igual que con sync.Mutex.
func (c *cerrojos) tomar(id string) func() {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*cerrojo{}
	}
	e := c.m[id]
	if e == nil {
		e = &cerrojo{}
		c.m[id] = e
	}
	// Se cuenta ANTES de bloquear: si se contara despues, la entrada podria
	// retirarse mientras esperamos, y estariamos esperando un mutex que ya no
	// es el de nadie.
	e.refs++
	c.mu.Unlock()

	e.mu.Lock()

	return func() {
		e.mu.Unlock()
		c.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(c.m, id)
		}
		c.mu.Unlock()
	}
}

// vivos dice cuantas entradas hay. Solo para pruebas y diagnostico: si esto
// crece sin parar, algo no esta soltando.
func (c *cerrojos) vivos() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
