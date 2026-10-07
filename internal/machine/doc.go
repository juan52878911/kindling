// Package machine gestiona el ciclo de vida de las microVMs del daemon:
// arranque en frío (Run), restauración desde un snapshot (runFrom), Freeze,
// Thaw, Pause, Squeeze, Resize, Commit, Stop y Remove, más el vigilante que
// reconcilia lo que dice el estado con lo que de verdad corre en el host.
//
// # Cerrojos y su orden
//
// El Manager tiene varios dominios de exclusión. Cuando hay que tomar más de
// uno a la vez, se toman SIEMPRE en este orden, y nunca al revés:
//
//	lifecycle(id)  →  shareSup.mu  →  m.mu  →  stateMu
//
// Qué protege cada uno:
//
//   - lifecycle(id) (m.lock / m.tryLock, ver cerrojos.go) serializa las
//     operaciones de ciclo de vida sobre UNA máquina: Run y runFrom desde que
//     la publican en byID hasta que queda running (o fallida), Freeze, Thaw,
//     Pause/Resume, Squeeze, Resize, PutMMDS, Commit (el de la plantilla),
//     Stop y Remove. Es el más externo
//     porque se sostiene durante operaciones lentas (arrancar un VMM, volcar la
//     memoria) en las que se toman y sueltan los demás muchas veces.
//   - shareSup.mu protege el registro de conexiones de carpetas vivas. Hoy no
//     se anida con m.mu en ningún sitio (startSharesIf lee la máquina con
//     m.get ANTES de tomarlo); si alguna vez hiciera falta, m.mu va dentro.
//   - m.mu (RWMutex) protege byID, socket, reserved, volReservas, pendingMiB,
//     volcandoMiB y demás mapas del Manager, y TODOS los campos de las *api.Machine vivas.
//     Nunca se sostiene durante E/S lenta ni llamadas al VMM; un rename a la
//     papelera sí (sweepMachineDirs, removeSnapshot), porque es instantáneo y
//     es lo que hace atómico "nadie lo usa, fuera". Las reservas de
//     reserveDir se cuentan: "snap:<nombre>" lo tienen a la vez el commit que
//     lo escribe y cada runFrom que lo lee, y removeSnapshot se niega
//     mientras haya alguna ajena.
//   - stateMu protege la foto pendiente de state.json. persist() lo toma con
//     m.mu ya tomado; writePending lo suelta antes de tomar escrituraMu y de
//     escribir, así que escrituraMu nunca está dentro de m.mu.
//
// metaMu (meta.json de snapshots), templateMu (plantilla de overlay) y el mutex
// interno de cerrojos son hojas: no se toma ningún otro cerrojo dentro de ellos.
//
// Dos cerrojos de ciclo de vida de máquinas DISTINTAS no se esperan nunca a la
// vez. Quien ya tiene uno y quiere otra máquina usa tryLock y se la salta si
// está ocupada (makeRoom, soltarRedesDormidas). Por eso Run y runFrom toman el
// suyo justo antes de publicar la máquina en byID y no antes: lo previo
// (checkMachineLimit → gcFailed → Remove de OTRA máquina) sí puede esperar el
// cerrojo de otra, y hasta publicarla nadie puede nombrar el id nuevo, así que
// tomarlo antes no protegería nada.
//
// El cerrojo de ciclo de vida NO es reentrante. Una operación que ya lo tiene
// y necesita otra operación pública sobre la misma máquina (Run, cuando fallan
// sus carpetas vivas y llama a Remove) tiene que soltarlo antes; por eso Run
// lo suelta con una función idempotente (lockUnaVez) y la ejecuta antes del
// Remove además de en su defer.
//
// Consecuencia visible: un Stop, Remove, Freeze o el TTL sobre una máquina que
// está arrancando esperan a que termine el arranque, en vez de actuar sobre una
// máquina a medio construir. Al conseguir el cerrojo, cada operación vuelve a
// leer la máquina: lo que vio antes de esperar (estado, NetIndex, PID) puede
// haber cambiado mientras tanto.
//
// # Puntero vivo frente a copia
//
// byID guarda punteros *api.Machine VIVOS; Get, get y List devuelven COPIAS.
// Las reglas:
//
//   - Un campo de una máquina viva solo se escribe con m.mu tomado, y a
//     través del puntero de byID (m.byID[id]), nunca a través de una copia:
//     escribir en una copia no cambia nada y el error no se ve (M-01, donde
//     fail() marcaba fallida una copia y la viva seguía "running").
//   - fail() recibe cualquiera de las dos y escribe SIEMPRE en la entrada viva
//     de byID; solo si ya no está registrada cae sobre la que le pasaron.
//   - Las copias son superficiales. Un slice o mapa de una máquina (Volumes,
//     Shares, Forwards, AllowDomains, Labels) nunca se modifica en su sitio:
//     se construye uno nuevo y se asigna bajo m.mu (ver withDriveIDs). persist()
//     guarda Clone() de cada máquina, no una copia por valor, porque la foto se
//     serializa fuera de m.mu.
//   - Leer sin m.mu un campo del puntero vivo solo es válido para quien tiene
//     el cerrojo de ciclo de vida de esa máquina y es el único que lo escribe
//     (Run y runFrom con la máquina que están creando).
package machine
