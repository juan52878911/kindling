package api

// El snapshot por imagen (docs/snapshot-imagen.md): `kling run -image X`
// restaura un snapshot de X tomado con el agente listo y NADA de la máquina
// dentro —ni entorno, ni comando, ni servicio arrancado—, y le da lo suyo
// después, como a un arranque en frío.
//
// Cómo lo pide el host: arranca la máquina de preparación con
// ImageSnapshotBootParam=1 en la línea del kernel. Un agente que lo entiende
// y puede cumplirlo (sin /entrypoint propio, sin init de la imagen como PID 1)
// se queda escuchando sin lanzar el servicio ni leer MMDS, y lo anuncia con
// GuestCapLateStart en /healthz. El host lo congela así. Cada copia, tras
// restaurarse y resincronizarse, recibe POST GuestStartPath con su entorno y
// su comando, y solo entonces el agente lanza el servicio.
//
// Un agente que no lo entiende ignora el parámetro (GuestCapBootOpt), arranca
// como siempre y no anuncia la capacidad: el host tira la máquina de
// preparación y no vuelve a intentarlo con esa imagen.

// ImageSnapshotBootParam (kling.imgsnap=1): arranque de preparación de un
// snapshot por imagen.
const ImageSnapshotBootParam = "kling.imgsnap"

// GuestCapLateStart: el agente arrancó con ImageSnapshotBootParam=1, no ha
// lanzado nada y espera POST GuestStartPath. Solo se anuncia en ese modo.
const GuestCapLateStart = "late-start"

// GuestStartPath (POST, cuerpo GuestStart): lo propio de la máquina a un
// agente que espera (GuestCapLateStart). Una vez: la segunda da 409. 204 si se
// aplicó y el servicio se lanzó (o no hay servicio). Ruta de control.
const GuestStartPath = "/machine/start"

// GuestStartMaxBody acota el cuerpo de GuestStartPath: el entorno entero en
// su tope más el comando, con holgura para el JSON.
const GuestStartMaxBody = 2*MaxMachineEnvBytes + 64<<10

// GuestStart es el cuerpo de POST GuestStartPath: el entorno de la máquina
// (RunRequest.Env, ya validado por el daemon) y su comando (RunRequest
// Entrypoint/Cmd). Los dos pueden faltar. Viaja por el canal del agente, que
// solo alcanza el host, y el agente lo guarda solo en su memoria, como el que
// lee de MMDS en un arranque en frío.
type GuestStart struct {
	Env map[string]string `json:"env,omitempty"`
	Cmd *MachineCmd       `json:"cmd,omitempty"`
}
