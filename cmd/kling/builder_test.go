package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El constructor base corre como root: todo lo que llega al script tiene que
// pasar antes por aquí.
func TestValidateBase(t *testing.T) {
	ok := api.BuildImageRequest{Name: "toolchain", Base: "min", GrowMB: 1536}
	if err := validateBase(ok, BaseSpec{Packages: []string{"nodejs", "py3-pip", "g++"}, Env: []string{"A=b c"}}); err != nil {
		t.Fatalf("petición válida rechazada: %v", err)
	}
	malos := []struct {
		req  api.BuildImageRequest
		spec BaseSpec
		want string
	}{
		{api.BuildImageRequest{Name: "../x"}, BaseSpec{}, "invalid image name"},
		{api.BuildImageRequest{Name: "x", Base: "$(id)"}, BaseSpec{}, "invalid base"},
		{api.BuildImageRequest{Name: "x", GrowMB: -1}, BaseSpec{}, "grow_mb"},
		{api.BuildImageRequest{Name: "x"}, BaseSpec{Packages: []string{"-y"}}, "invalid package"},
		{api.BuildImageRequest{Name: "x"}, BaseSpec{Packages: []string{"a;rm -rf /"}}, "invalid package"},
		{api.BuildImageRequest{Name: "x"}, BaseSpec{Env: []string{"A=b\nexec sh"}}, "invalid env"},
		{api.BuildImageRequest{Name: "x"}, BaseSpec{Env: []string{"1A=b"}}, "invalid env"},
	}
	for _, m := range malos {
		if err := validateBase(m.req, m.spec); err == nil || !strings.Contains(err.Error(), m.want) {
			t.Errorf("%+v %+v: quería %q, salió %v", m.req, m.spec, m.want, err)
		}
	}
}
