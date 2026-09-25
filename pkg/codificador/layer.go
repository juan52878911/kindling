package codificador

import (
	"context"
	"fmt"
	"time"
)

// Layer es la capa 3 lista para la cascada: el codificador (Embedder) y la
// cabeza entrenada sobre SUS vectores. Implementa domotica.IntentEncoder.
type Layer struct {
	Head     *Head
	Embedder Embedder
	// Timeout acota una petición al codificador (0 = 2 s). Sin respuesta, la
	// cascada escala a la capa siguiente en vez de esperar.
	Timeout time.Duration
}

// Classify pide el vector de text (con el prefijo que espera el
// codificador) y lo pasa por la cabeza.
func (l *Layer) Classify(ctx context.Context, text string) (Prediction, error) {
	to := l.Timeout
	if to <= 0 {
		to = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	vs, err := l.Embedder.Embed(ctx, []string{l.Head.Meta.Prefix + text})
	if err != nil {
		return Prediction{}, err
	}
	if len(vs) != 1 {
		return Prediction{}, fmt.Errorf("encoder returned %d vectors for one text", len(vs))
	}
	return l.Head.Predict(vs[0])
}

// ClassifyIntent es Classify con tipos sencillos, para que pkg/domotica no
// dependa de este paquete.
func (l *Layer) ClassifyIntent(ctx context.Context, text string) (string, float64, bool, error) {
	p, err := l.Classify(ctx, text)
	if err != nil {
		return "", 0, false, err
	}
	return p.Label, p.Prob, p.Confident, nil
}
