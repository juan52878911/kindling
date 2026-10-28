package main

import (
	"reflect"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestPublishFlag(t *testing.T) {
	var p publishFlag
	for _, v := range []string{"8080:80", "5432", "127.0.0.1:9229:9229", "6000:6379/tcp"} {
		if err := p.Set(v); err != nil {
			t.Fatalf("-p %s: %v", v, err)
		}
	}
	if want := (publishFlag{{8080, 80}, {5432, 5432}, {9229, 9229}, {6000, 6379}}); !reflect.DeepEqual(p, want) {
		t.Errorf("-p = %v", p)
	}
	for _, v := range []string{"0.0.0.0:80:80", "a:80", "1:2:3:4", "70000:1"} {
		if err := p.Set(v); err == nil {
			t.Errorf("-p %s accepted", v)
		}
	}
}

func TestConPuertos(t *testing.T) {
	got := conPuertos(map[string]string{api.LabelPorts: "9000, 80", "x": "y"}, [][2]int{{8080, 80}, {3001, 3000}})
	if got[api.LabelPorts] != "80,3000,9000" || got["x"] != "y" {
		t.Errorf("labels = %v", got)
	}
	if conPuertos(nil, nil) != nil {
		t.Error("without -p the labels change")
	}
}
