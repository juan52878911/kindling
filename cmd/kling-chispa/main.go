// kling-chispa es el servicio de Chispa dentro de una microVM: carga un .chispa (y,
// opcional, un .chispas de huecos) al arrancar y sirve POST /v1/classify y
// GET /healthz en el puerto del invitado.
//
// No es el PID 1 de la microVM —eso lo sigue siendo kling-guest, que lo
// arranca y lo relanza si muere (SERVICE de scripts/81-base-image.sh, ver
// cmd/kling/builder_chispa.go)—: kling-chispa solo sabe de Chispa, igual que
// llama-server solo sabe de generar texto en una imagen VON. Así el dorado
// congelado de una tarea es exactamente eso: un proceso con el modelo ya
// cargado, listo para que `kling ai serve` lo despierte con la primera
// petición (docs/chispa-serverless.md).
//
// Estático, sin cgo ni dependencias externas (igual que pkg/chispa): un solo
// binario por arquitectura basta para la imagen mínima.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Version se fija al compilar: -ldflags "-X main.Version=..."
var Version = "dev"

// maxBody es el cuerpo máximo de una petición: el mismo límite que usa el
// gateway de IA para el texto de una tarea (pkg/aigw.maxText), para que un
// cliente no note la diferencia si habla directamente con la réplica.
const maxBody = 64 << 10

func main() {
	listen := flag.String("listen", ":8000", "where to listen (the guest port the daemon proxies)")
	modelPath := flag.String("model", "/models/task.chispa", "path to the .chispa model")
	slotsPath := flag.String("slots", "", "optional path to a .chispas slot model")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "kling-chispa — a Chispa model as a kindling guest service\n\n  kling-chispa -model m.chispa [-slots s.chispas] [-listen :8000]\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *version {
		fmt.Println(Version)
		return
	}

	m, err := chispa.LoadFile(*modelPath)
	if err != nil {
		log.Fatalf("loading %s: %v", *modelPath, err)
	}
	var sm *slots.Model
	if *slotsPath != "" {
		if sm, err = slots.LoadFile(*slotsPath); err != nil {
			log.Fatalf("loading %s: %v", *slotsPath, err)
		}
	}
	note := ""
	if sm != nil {
		note = fmt.Sprintf(" + slots %s (%d tags)", *slotsPath, len(sm.SlotNames()))
	}
	log.Printf("kling-chispa %s: loaded %s (%d labels)%s", Version, *modelPath, len(m.Labels), note)

	srv := &server{model: m, slots: sm}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.HandleFunc("/v1/classify", srv.handleClassify)

	// PID 1 es kling-guest, no este proceso: SIGTERM aquí es "el segador te
	// está congelando o el host te para", y basta con cerrar limpio el
	// servidor. No hace falta recoger huérfanos ni desmontar volúmenes.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	hs := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
		close(done)
	}()

	log.Printf("kling-chispa %s listening on %s", Version, *listen)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
}

// server sirve las peticiones de clasificación. m es inmutable tras cargarlo
// (pkg/chispa.Model), así que no hace falta ningún candado.
type server struct {
	model *chispa.Model
	slots *slots.Model
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

// classifyRequest es el cuerpo de POST /v1/classify: el mismo esquema que
// pkg/aigw.ClassifyRequest usa para hablar con Chispa (text, fields), para que el
// gateway pueda reenviar la petición tal cual cuando la tarea vive en una
// microVM en vez de en su propio proceso.
type classifyRequest struct {
	Text   string         `json:"text"`
	Fields map[string]any `json:"fields,omitempty"`
	// Explain pide la distribución completa y la evidencia también cuando Chispa
	// contesta seguro (cuesta reservas de memoria); el gateway la usa para
	// depurar o cuando el cliente pide explain=true (pkg/aigw.ClassifyRequest).
	Explain bool `json:"explain,omitempty"`
}

// classifyResponse es la respuesta: la predicción de Chispa, y los huecos si hay
// modelo de huecos. La cascada (escalar a VON, aplicar umbrales por tarea) la
// decide quien llama —el gateway—, no esta réplica: aquí solo se sirve lo que
// dice el modelo.
//
// Candidates trae SIEMPRE la distribución ENTERA (no un top-N) cuando va: es
// lo mismo que pkg/chispa.Model.PredictFull da en proceso, para que el gateway
// pueda tratar una tarea con backend "microvm" exactamente igual que una en
// proceso (top_k, la plantilla de la cascada, etc. sin perder etiquetas).
type classifyResponse struct {
	Label      string             `json:"label"`
	Prob       float64            `json:"prob"`
	Threshold  float64            `json:"threshold"`
	Confident  bool               `json:"confident"`
	Decision   string             `json:"decision"`
	Candidates []chispa.ClassProb `json:"candidates,omitempty"`
	Evidence   []chispa.Evidence  `json:"evidence,omitempty"`
	Slots      []slots.Span       `json:"slots,omitempty"`
	LatencyUS  float64            `json:"latency_us"`
}

func (s *server) handleClassify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	t0 := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req classifyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Text) > maxBody {
		http.Error(w, "text too large", http.StatusRequestEntityTooLarge)
		return
	}

	in := chispa.Input{Text: req.Text, Fields: req.Fields}
	// Camino rápido: Predict no reserva memoria. Solo se pide la distribución
	// completa (PredictFull, con reservas) cuando Chispa duda o el cliente pide
	// explain: es la misma regla que sigue el gateway en proceso
	// (pkg/aigw.Classify), en una sola vuelta en vez de dos.
	p := s.model.Predict(in)
	resp := classifyResponse{
		Label: p.Label, Prob: p.Prob, Threshold: p.Threshold,
		Confident: p.Confident, Decision: p.Decision,
	}
	if !p.Confident || req.Explain {
		full := s.model.PredictFull(in, 5)
		resp.Evidence = full.Evidence
		resp.Candidates = full.Probs // todas las etiquetas, ya ordenadas por probabilidad
	}
	if s.slots != nil {
		resp.Slots = s.slots.Tag(req.Text, nil)
	}
	resp.LatencyUS = float64(time.Since(t0).Microseconds())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
