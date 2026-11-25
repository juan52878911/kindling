# Plan de lanzamiento: 3 semanas

Borrador para revisar. Semana 1: preparar. Semana 2: lanzar. Semana 3: seguimiento. Las horas
están en hora de España (CET); HN y Reddit en inglés se mueven con la mañana de EE. UU.

## Semana 1: antes (no lanzar hasta cerrar esto)

**Lo primero: que lo que se va a citar sea verdad y reproducible.**

- [ ] **Datos crudos del banco.** Subir el JSON de la corrida del 2026-11-21 a
  `docs/bench-data/` y rellenar en `article.md` el commit, `net_pool` y el jailer de esa
  corrida. Hoy no están en el repo, y la salvedad 7 dice que se citen con las cifras.
- [ ] **Logs de OpenSandbox.** Repetir su corrida y guardar los logs. Con lo que hay ahora, «no
  están en el repositorio» es lo primero que preguntarán.
- [ ] **Shim de E2B.** Resolver la contradicción: el CHANGELOG dice «works with the real E2B
  SDKs» y `docs/e2b.md` dice «No se ha probado con el SDK real». Esperar a la verificación en
  curso y dejar un solo mensaje en los dos sitios.
- [ ] **`docs/quickstart-sandbox.md`.** Su sección «Coming from OpenSandbox» dice que el shim
  habla E2B y no la API de OpenSandbox, pero `kling sbx osb` ya existe. Corregirlo.
- [ ] **Quickstart de punta a punta.** Ejecutarlo en un host KVM limpio (Ubuntu actual,
  amd64) y quitar el aviso de «no se ha corrido de punta a punta», o dejarlo si falla algo.
- [ ] **Instalación limpia.** Probar `install.sh` + `kling try -- uname -a` en un Linux sin
  nada instalado y en un Mac con Apple Silicon, y cronometrar cuánto tarda el primer `try`
  (descarga incluida) para saber qué esperar.
- [ ] **Versión.** Decidir si se lanza con `v1.0.0-rc.3` o se publica `v1.0.0` antes. Que el
  README, el artículo y los posts digan lo mismo.

**Lo segundo: el público de HN lee en inglés.**

- [ ] **Resumen en inglés de `SECURITY.md`.** Al menos un resumen de una página del modelo de
  amenaza y de «Lo que NO está resuelto». Es lo que más se va a leer, y hoy está en español.
- [ ] **Sección «Who/why» en el README.** Una línea con quién lo mantiene, por qué existe y
  el modelo de negocio, aunque sea «ninguno».
- [ ] **Demo.** Un asciinema o GIF de 30 s: `kling try`, `freeze`, `thaw`, y un sandbox del
  pool. Sin cortes que escondan esperas.
- [ ] **El repo, preparado para recibir gente:** Discussions activadas, plantillas de issue
  (bug con salida de `kling doctor`, informe de benchmark), etiqueta `good first issue` en 3–5
  issues reales, y Private Vulnerability Reporting activo.
- [ ] **Documentación.** Comprobar que kindling.asccilabs.com carga y que los enlaces del
  artículo funcionan.
- [ ] **Invitación a Lobsters.** Pedirla ya, si no la tienes. Participar unos días antes de
  enviar nada tuyo.
- [ ] **Rellenar los `<PLACEHOLDER>`** de los cuatro ficheros, sobre todo nombre, modelo de
  negocio y cómo se usaron asistentes de IA en el desarrollo. Las tres cosas las van a
  preguntar.

## Semana 2: el lanzamiento

**Día 1 (martes, miércoles o jueves). Show HN.**

- Publicar entre las **14:00 y las 16:00 CET** (08:00–10:00 en la costa este de EE. UU.).
  Evitar viernes, fines de semana y festivos de EE. UU.
- Publicar el primer comentario (`show-hn.md`) en el primer minuto.
- **Reservar 6–8 horas seguidas** para contestar. Las primeras dos horas deciden casi todo.
- No pedir votos a nadie, ni en privado. Se puede compartir el enlace, pero sin «vota».
- Si no despega, no borrar y volver a publicar el mismo día. HN permite un segundo intento
  pasado un tiempo, si hay algo nuevo. También se puede escribir a los moderadores
  (hn@ycombinator.com) para pedir la «second-chance pool», sin insistir.

**Día 2–3. Lobsters**, con el artículo, no con el repo. Marca «I am the author». Solo si
tienes invitación y algo de historial allí.

**Día 3–5. Reddit.** Un subreddit por día, no todos a la vez: r/selfhosted, luego
r/LocalLLaMA. Releer las reglas ese mismo día. Contestar todos los comentarios del primer día.

**Día 4–6. X/Bluesky** (el hilo) y **LinkedIn** en español. Comunidades en español a lo largo
de la semana, respetando sus hilos de autopromoción.

## Cómo responder a las críticas

- **Primero, dar la razón en lo que la tenga.** «Tienes razón, n=15 es poco; por eso doy
  rangos y no una cifra.» Las salvedades ya están escritas: enlázalas, no las repitas a la
  defensiva.
- **Benchmarks.** No discutir. Si alguien ve un fallo en la configuración de OpenSandbox,
  agradecerlo, abrir un issue, rehacer la corrida y actualizar la tabla aunque salga peor.
  Decirlo en el hilo cuando esté hecho.
- **«Esto ya existe (Kata/E2B/Fly…)».** Explicar la diferencia concreta (autoalojado,
  diseñado alrededor de snapshots, congelar a disco, el mismo CLI en Linux y Mac) sin
  menospreciar el otro proyecto. Si no lo has medido, decir que no lo has medido.
- **Seguridad.** Un posible fallo se trata como un informe: agradecerlo y pedir que lo manden
  por el canal privado si es explotable. Nunca minimizarlo en público.
- **IA en el desarrollo.** Contestar con hechos: cómo se usó, cómo se revisa y qué tests hay.
  Sin ponerse a la defensiva.
- **Tono.** Sin sarcasmo, aunque el comentario lo tenga. Si un hilo se vuelve tóxico, una
  respuesta correcta y no seguir.
- Apunta cada crítica válida en un issue con la etiqueta `launch-feedback`. Te dará la
  semana 3.

## Métricas a vigilar

- **Uso real:** descargas de los assets de la release (`gh release view --json assets`),
  issues de instalación, informes de `kling try` en otro hardware.
- **Tráfico:** GitHub Insights → Traffic (visitantes únicos, clones, referentes). Mirarlo el
  día 1, el día 3 y el día 7.
- **Conversación:** comentarios en HN y su posición (sin obsesionarse), preguntas repetidas
  (son agujeros en la documentación), primeras PR de terceros.
- **Estrellas:** se pueden mirar, pero no son la métrica. Lo que vale es la gente que lo
  instala y vuelve con un issue.
- **Lo que hay que corregir:** cuántos issues `launch-feedback` se abren y cuántos se cierran
  en la semana 3.

## Semana 3: después

- [ ] **Resumen de lo aprendido** (issue o Discussion fijada): preguntas frecuentes, qué se
  corrigió y qué queda pendiente.
- [ ] **Arreglar primero lo que más se repitió** (instalación y documentación), y sacar una
  release de parche si hace falta.
- **Posts de seguimiento.** Uno cada 1–2 semanas, cada uno con un resultado nuevo y medido,
  no con un anuncio:
  1. **El exec de ~1 s de OpenSandbox:** qué lo causaba, cuando se mida (E3). Si era un fallo
     de mi configuración, decirlo.
  2. **Pausa frente a congelación**, a fondo: el desglose por fases de `docs/despertar.md`,
     en inglés.
  3. **La matriz de thaw a escala en un host con más RAM:** cerrar las celdas de «50
     servicios × 200 agentes» que hoy están `skipped`.
  4. **Una corrida en bare metal y con OpenSandbox sobre Kata o gVisor**, para quitar las
     salvedades 1 y 3.
  5. **El barrido de la puerta de arranque** (2/4/8/16) con el desglose cola/trabajo del c50.
- [ ] Contestar los comentarios que lleguen tarde (HN y Reddit siguen recibiendo días
  después).
