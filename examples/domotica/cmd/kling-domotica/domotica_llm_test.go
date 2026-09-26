package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/domotica"
)

// La tarea de generación de la capa 4 tiene que ser válida para el gateway
// (es la que se copia a ai.json).
func TestLLMTaskConfigValid(t *testing.T) {
	cfg := &aigw.Config{
		Models: map[string]*aigw.ModelConfig{"g": {Kind: aigw.KindVON, Snapshot: "g"}},
		Tasks:  map[string]*aigw.TaskConfig{llmTaskName: llmTaskConfig("g")},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if *cfg.Tasks[llmTaskName].Temperature != 0 || !strings.Contains(string(cfg.Tasks[llmTaskName].JSONSchema), `"kind"`) {
		t.Fatal("temperature 0 and the domotica schema")
	}
}

// Las órdenes de MASSIVE van todas; lo fuera de ámbito, en una muestra
// determinista con el peso que la devuelve a su proporción.
func TestMassiveL4Rows(t *testing.T) {
	var all []domotica.Row
	for i := 0; i < 10; i++ {
		all = append(all, domotica.Row{Text: "oos " + string(rune('a'+i)), Lang: "es", Intent: domotica.OutOfScope, Source: "massive"})
	}
	all = append(all, domotica.Row{Text: "enciende", Lang: "es", Intent: "turn_on", Source: "massive"},
		domotica.Row{Text: "ha", Lang: "es", Intent: "turn_on", Source: "ha"})
	rows := massiveL4Rows(all, 4)
	var in, oos int
	for _, r := range rows {
		switch r.Group {
		case "massive/es":
			in++
		case "massive-oos/es":
			oos++
			if r.Weight != 2.5 {
				t.Fatalf("weight %v", r.Weight)
			}
		default:
			t.Fatalf("group %s", r.Group)
		}
	}
	if in != 1 || oos != 4 {
		t.Fatalf("in %d oos %d", in, oos)
	}
	again := massiveL4Rows(all, 4)
	for i := range rows {
		if rows[i].Row.Text != again[i].Row.Text {
			t.Fatal("the sample must be deterministic")
		}
	}
}
