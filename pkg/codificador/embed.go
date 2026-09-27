// Package codificador es la capa 3 de la decisión de intención (pkg/intent): un codificador
// de frases pequeño (multilingual-e5-small o paraphrase-multilingual-MiniLM,
// servidos con los embeddings de llama.cpp desde una microVM) y, encima, una
// cabeza de clasificación entrenada sobre los embeddings congelados.
//
// Por qué una capa entre Chispa y el LLM: Chispa (n-gramas hasheados) solo sabe de
// las palabras que vio; un codificador de frases pone cerca «cancela mi pedido»
// y «I don't want the order anymore» aunque no compartan ni una palabra, y lo hace en
// milisegundos en CPU, dos órdenes de magnitud menos que un LLM. La cabeza es
// lo único que se entrena aquí (el codificador va congelado): una regresión
// logística multinomial, o un perceptrón de una capa oculta, en Go puro,
// determinista y cuantizada a int16, en un fichero .jenc endurecido como el
// .chispa.
//
// Nada de este paquete habla con el daemon: el codificador es una URL
// (llama-server con --embeddings) o una caché de embeddings ya calculados, que
// es lo que usan el entrenamiento y la evaluación para ser reproducibles.
package codificador

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

// Embedder convierte textos en vectores. Los textos llegan ya con el prefijo
// que pida el modelo («query: » en e5): quien llama lo añade.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Límites de una petición al codificador. El invitado no es de fiar: la
// respuesta se lee acotada y cada vector se valida antes de usarlo.
const (
	MaxBatch     = 256     // textos por petición
	MaxTextBytes = 4 << 10 // un texto más largo se recorta (una orden cabe de sobra)
	MaxDim       = 4096
	// maxAnswerPerText es lo que puede ocupar un vector en JSON: 4096
	// dimensiones a ~24 bytes cada una.
	maxAnswerPerText = MaxDim * 24
)

// HTTPEmbedder habla con llama-server (--embeddings) por su API compatible
// con OpenAI: POST {URL}/v1/embeddings.
type HTTPEmbedder struct {
	URL    string // http://host:puerto
	Client *http.Client
	// Dim, si no es 0, es la dimensión esperada: otra es un error (un
	// codificador distinto del de la cabeza daría intenciones basura).
	Dim int
}

// httpClient es el cliente por defecto: plazo corto para conectar y sin
// keep-alive, por lo mismo que el gateway (una réplica congelada y
// descongelada deja muertas las conexiones ociosas).
var httpClient = &http.Client{Transport: &http.Transport{
	DialContext:            (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	MaxResponseHeaderBytes: 64 << 10,
	DisableCompression:     true,
	DisableKeepAlives:      true,
}}

// EmbedBody es el cuerpo de POST /v1/embeddings para texts: recortados a
// MaxTextBytes y sin entradas vacías (llama-server las rechaza; un espacio da
// el vector de «nada», y la cabeza lo manda a donde tenga que mandarlo).
func EmbedBody(texts []string) ([]byte, error) {
	if len(texts) > MaxBatch {
		return nil, fmt.Errorf("at most %d texts per request, got %d", MaxBatch, len(texts))
	}
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = truncUTF8(t, MaxTextBytes)
		if strings.TrimSpace(in[i]) == "" {
			in[i] = " "
		}
	}
	return json.Marshal(struct {
		Input []string `json:"input"`
	}{in})
}

// Embed pide los vectores de texts (como mucho MaxBatch).
func (h *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := EmbedBody(texts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.URL, "/")+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := h.Client
	if c == nil {
		c = httpClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return ParseEmbeddings(resp.StatusCode, resp.Body, len(texts), h.Dim)
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// ParseEmbeddings lee la respuesta de /v1/embeddings (acotada) y comprueba que
// trae n vectores finitos de la misma dimensión (dim si no es 0), cada índice
// una vez. La usa también el gateway, que llega a la réplica por su cuenta.
func ParseEmbeddings(status int, r io.Reader, n, dim int) ([][]float32, error) {
	limit := int64(n)*maxAnswerPerText + 64<<10
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("encoder answer larger than %d bytes", limit)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("encoder answered %d: %s", status, truncUTF8(string(b), 200))
	}
	var out embedResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("encoder answer is not an embeddings response: %w", err)
	}
	if len(out.Data) != n {
		return nil, fmt.Errorf("encoder returned %d vectors for %d texts", len(out.Data), n)
	}
	vecs := make([][]float32, n)
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= n || vecs[d.Index] != nil {
			return nil, fmt.Errorf("encoder returned a bad or repeated index %d", d.Index)
		}
		if len(d.Embedding) == 0 || len(d.Embedding) > MaxDim || (dim > 0 && len(d.Embedding) != dim) {
			return nil, fmt.Errorf("encoder returned a vector of %d dimensions (want %d)", len(d.Embedding), dim)
		}
		if dim == 0 {
			dim = len(d.Embedding)
		}
		v := make([]float32, len(d.Embedding))
		for i, x := range d.Embedding {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, errors.New("encoder returned a non-finite value")
			}
			v[i] = float32(x)
		}
		vecs[d.Index] = v
	}
	return vecs, nil
}

// truncUTF8 recorta s a como mucho n bytes sin partir una runa.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
