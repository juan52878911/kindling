package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// tools/list pagina con nextCursor: 4 herramientas en 2 páginas son 4, no 2.
func TestIntrospectSiguePaginas(t *testing.T) {
	var cursores []string
	post := func(sid, body string) (string, []byte, error) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		switch req.Method {
		case "initialize":
			return "s1", []byte(`{"result":{"serverInfo":{"name":"x"}}}`), nil
		case "tools/list":
			cursores = append(cursores, req.Params.Cursor)
			if req.Params.Cursor == "" {
				return sid, []byte(`{"result":{"tools":[{"name":"a"},{"name":"b"}],"nextCursor":"p2"}}`), nil
			}
			return sid, []byte(`{"result":{"tools":[{"name":"c"},{"name":"d"}]}}`), nil
		}
		return sid, []byte(`{}`), nil
	}
	_, _, tools, err := introspectConSesion(post)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "a,b,c,d" || strings.Join(cursores, ",") != ",p2" {
		t.Fatalf("tools %v, cursors %q", names, cursores)
	}

	// Un servidor que repite el cursor no deja al CLI en un bucle.
	sinFin := func(sid, body string) (string, []byte, error) {
		if strings.Contains(body, `"initialize"`) {
			return "s1", []byte(`{"result":{}}`), nil
		}
		return sid, []byte(`{"result":{"tools":[{"name":"a"}],"nextCursor":"otra"}}`), nil
	}
	if _, _, _, err := introspectConSesion(sinFin); err == nil {
		t.Error("an endless cursor was accepted")
	}
}
