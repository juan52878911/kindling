package aigw

import (
	"context"

	"github.com/juan52878911/kindling/pkg/api"
)

// SEMILLA de la ronda 9 (analisis/ronda9-carga/plan-r9.md, pendiente 1 de la
// ronda 8). Dueño: Z4 (planificador, repuesto y secretos). Solo declaraciones.

// Formas de entregar un secreto a un backend (SecretConfig.Deliver).
const (
	// DeliverCredential: secreto de SALIDA (backend → tercero). El gateway no
	// entrega nada a la réplica: ata una credencial de plantilla al dorado
	// (proxy de credenciales del daemon) y la réplica solo lleva un marcador.
	// Se rota cambiando File y recargando, sin dorado nuevo. Exige Domain y
	// Allow.
	DeliverCredential = "credential"
	// DeliverMMDS: secreto que el proceso necesita EN CLARO. El gateway lo
	// pone en el almacén MMDS de cada réplica tras restaurarla
	// (scheduler.AfterRestore) y corre los ganchos de la imagen. La réplica
	// queda marcada (no se congela ni se bifurca): exige idle remove o pause.
	DeliverMMDS = "mmds"
)

// SecretConfig es un secreto de un backend tal como se escribe en ai.json
// ("secrets" de un modelo kind http con snapshot). El valor nunca va en el
// fichero: está en File.
type SecretConfig struct {
	// Name identifica el secreto dentro del bloque: clave del almacén MMDS
	// ("secrets".<name>) o nombre de la credencial. [a-z0-9_-]+, único.
	Name string `json:"name"`
	// Deliver es una de las constantes Deliver*.
	Deliver string `json:"deliver"`
	// File es el fichero con el valor: relativo al ai.json, 0600, como
	// Auth.File. Se lee al usarlo; su hash es la "generación" con la que se
	// detecta una rotación en Reload.
	File string `json:"file"`
	// Env es la variable del servicio que lleva el marcador (solo
	// DeliverCredential).
	Env string `json:"env,omitempty"`
	// Domain es el nombre al que el backend dirige la petición y que el proxy
	// de credenciales intercepta (solo DeliverCredential).
	Domain string `json:"domain,omitempty"`
	// Upstream es el destino fijado por el operador, "host:puerto" (solo
	// DeliverCredential; necesita la credencial HTTP con upstream de Z6). Vacío
	// = el propio Domain, que tiene que ser público.
	Upstream string `json:"upstream,omitempty"`
	// Allow son las peticiones "MÉTODO /ruta" que el proxy deja pasar con la
	// credencial; obligatorio con DeliverCredential (el proxy es un oráculo de
	// la clave frente al invitado).
	Allow []string `json:"allow,omitempty"`
}

// validateSecrets valida el bloque "secrets" del modelo n. Semilla: no valida
// nada (un ai.json sin "secrets" es lo de hoy). Z4 lo rellena: solo kind http
// con snapshot, Name único, Deliver conocido, File 0600 (como checkAuth),
// DeliverMMDS con idle remove o pause, DeliverCredential con Domain y Allow.
func validateSecrets(n string, m *ModelConfig) []error {
	return nil
}

// Declaraciones para los tests de la ronda 9 (Z4); se rellenan después.
func (g *Gateway) aplicarSecretos(ctx context.Context) {}
func (g *Gateway) entregarSecretos(ctx context.Context, snap string, mc *api.Machine) error {
	return nil
}
