package net

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// iptablesFalso simula las cadenas del host para SetupHost, setupHostInput y
// setupAcceso: -C, -D, -I 1, -A, -N, el -n -L que dice si una cadena existe y
// el -S de listarCadena. Las de filter van por su nombre; las de otra tabla,
// como "tabla/CADENA". Existen siempre INPUT, OUTPUT, FORWARD, nat/POSTROUTING
// y mangle/PREROUTING; las demás, tras su -N.
type iptablesFalso struct {
	mu    sync.Mutex
	input []string
	otras map[string][]string
	// falla, si no es nil, decide qué ejecutar falla (como un iptables que
	// no entiende una opción).
	falla func(args []string) error
}

var cadenasFijas = []string{"OUTPUT", "FORWARD", "nat/POSTROUTING", "mangle/PREROUTING"}

func (f *iptablesFalso) cadena(nombre string) ([]string, bool) {
	if nombre == "INPUT" {
		return f.input, true
	}
	c, ok := f.otras[nombre]
	return c, ok || slices.Contains(cadenasFijas, nombre)
}

func (f *iptablesFalso) poner(nombre string, c []string) {
	if nombre == "INPUT" {
		f.input = c
		return
	}
	if f.otras == nil {
		f.otras = map[string][]string{}
	}
	f.otras[nombre] = c
}

// partir separa "iptables [-t T] ACCION CADENA resto" en la acción, la cadena
// (con su tabla delante si no es filter) y el resto.
func partir(args []string) (accion, cadena string, resto []string) {
	a := args[1:]
	tabla := ""
	if a[0] == "-t" {
		if a[1] != "filter" {
			tabla = a[1] + "/"
		}
		a = a[2:]
	}
	return a[0], tabla + a[1], a[2:]
}

func (f *iptablesFalso) comprobar(args []string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args[1] == "-n" && args[2] == "-L" {
		_, ok := f.cadena(args[3])
		return ok
	}
	_, nombre, resto := partir(args)
	c, _ := f.cadena(nombre)
	return slices.Contains(c, strings.Join(resto, " "))
}

func (f *iptablesFalso) estado(args []string) (bool, error) { return f.comprobar(args), nil }

func (f *iptablesFalso) listar(nombre string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cadena(nombre)
	if !ok {
		return nil, errors.New("no existe la cadena")
	}
	var out []string
	for _, r := range c {
		out = append(out, "-A "+nombre+" "+r)
	}
	return out, nil
}

func (f *iptablesFalso) ejecutar(args ...string) error {
	if f.falla != nil {
		if err := f.falla(args); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if args[0] != "iptables" {
		return nil // el sh del ip_forward y lo que no sea iptables
	}
	accion, nombre, resto := partir(args)
	if accion == "-N" {
		if _, ok := f.cadena(nombre); ok {
			return errors.New("ya existe")
		}
		f.poner(nombre, []string{})
		return nil
	}
	c, ok := f.cadena(nombre)
	if !ok {
		return fmt.Errorf("no existe la cadena %s", nombre)
	}
	switch accion {
	case "-D":
		if i := slices.Index(c, strings.Join(resto, " ")); i >= 0 {
			f.poner(nombre, slices.Delete(c, i, i+1))
			return nil
		}
		return errors.New("no existe")
	case "-I":
		f.poner(nombre, slices.Insert(c, 0, strings.Join(resto[1:], " ")))
		return nil
	case "-A":
		f.poner(nombre, append(c, strings.Join(resto, " ")))
		return nil
	}
	return fmt.Errorf("inesperado: %v", args)
}

// usarIptablesFalso pone f en lugar de iptables y de ip_forward hasta el
// final del test, y deja el estado de SetupHost como estaba.
func usarIptablesFalso(t interface{ Cleanup(func()) }, f *iptablesFalso) {
	oldC, oldE, oldL, oldR, oldT, oldG := comprobarEstado, ejecutar, listarCadena, activarReenvio, listarTabla, guardarTablas
	t.Cleanup(func() {
		comprobarEstado, ejecutar, listarCadena, activarReenvio, listarTabla, guardarTablas = oldC, oldE, oldL, oldR, oldT, oldG
		inputMontado.Store(false)
		accesoMontado.Store(nil)
		accesoPedido.Store(nil)
	})
	comprobarEstado, ejecutar, listarCadena = f.estado, f.ejecutar, f.listar
	listarTabla = func(tabla, cadena string) ([]string, error) {
		if tabla != "filter" {
			cadena = tabla + "/" + cadena
		}
		return f.listar(cadena)
	}
	activarReenvio = func() error { return nil }
	// Sin iptables-save por defecto: el camino lento, que cuenta cada
	// comprobación. Los tests de la instantánea lo sustituyen.
	guardarTablas = func() (string, error) { return "", errors.New("sin iptables-save") }
}

// guardado es lo que iptables-save diría del estado del falso.
func (f *iptablesFalso) guardado() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	tablas := map[string][]string{}
	for _, r := range f.input {
		tablas["filter"] = append(tablas["filter"], "-A INPUT "+r)
	}
	for nombre, reglas := range f.otras {
		tabla, cadena := "filter", nombre
		if t, c, ok := strings.Cut(nombre, "/"); ok {
			tabla, cadena = t, c
		}
		for _, r := range reglas {
			tablas[tabla] = append(tablas[tabla], "-A "+cadena+" "+r)
		}
	}
	for t, ls := range tablas {
		fmt.Fprintf(&b, "*%s\n%s\nCOMMIT\n", t, strings.Join(ls, "\n"))
	}
	return b.String()
}
