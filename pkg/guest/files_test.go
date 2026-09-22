package guest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func files(method, p string, q url.Values, body string) *httptest.ResponseRecorder {
	if q == nil {
		q = url.Values{}
	}
	q.Set("path", p)
	rec := httptest.NewRecorder()
	FilesHandler()(rec, httptest.NewRequest(method, "/files?"+q.Encode(), strings.NewReader(body)))
	return rec
}

func TestFilesPutGetStatDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "a.sh")

	if rec := files(http.MethodPut, p, nil, "x"); rec.Code == http.StatusOK {
		t.Fatal("PUT into a missing directory without mkdir should fail")
	}
	rec := files(http.MethodPut, p, url.Values{"mode": {"0755"}, "mkdir": {"1"}}, "#!/bin/sh\necho hi\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %d: %s", rec.Code, rec.Body.String())
	}
	var st api.FileStat
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Mode != "0755" || st.Size != 18 {
		t.Fatalf("stat after put %+v", st)
	}
	if rec := files(http.MethodGet, p, nil, ""); rec.Code != http.StatusOK || rec.Body.String() != "#!/bin/sh\necho hi\n" {
		t.Fatalf("GET %d %q", rec.Code, rec.Body.String())
	}
	if rec := files(http.MethodGet, p, url.Values{"stat": {"1"}}, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"0755"`) {
		t.Fatalf("stat %d %s", rec.Code, rec.Body.String())
	}
	if rec := files(http.MethodDelete, p, nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE %d", rec.Code)
	}
	if rec := files(http.MethodGet, p, nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete %d", rec.Code)
	}
}

func TestFilesNoSigueEnlaces(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secreto")
	_ = os.WriteFile(target, []byte("s"), 0o600)
	link := filepath.Join(dir, "enlace")
	_ = os.Symlink(target, link)

	if rec := files(http.MethodGet, link, nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("GET through symlink: %d", rec.Code)
	}
	if rec := files(http.MethodPut, link, nil, "pisado"); rec.Code == http.StatusOK {
		t.Fatal("PUT through a symlink should be refused")
	}
	if b, _ := os.ReadFile(target); string(b) != "s" {
		t.Fatalf("target was overwritten: %q", b)
	}
}

func TestFilesRutaRelativaYMetodo(t *testing.T) {
	if rec := files(http.MethodGet, "relativa", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("relative path: %d", rec.Code)
	}
	if rec := files(http.MethodPost, "/tmp/x", nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}
