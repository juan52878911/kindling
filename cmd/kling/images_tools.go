package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"github.com/juan52878911/kindling/pkg/api"
)

// BuildToolsImage es la imagen de herramientas donde `image import -isolate`
// aplana las capas: la base min con este mismo binario en
// /usr/local/bin/kling, que dentro de la VM corre el constructor oci sin red.
const BuildToolsImage = "kling-build-tools"

// imagesBuildTools construye BuildToolsImage. El binario sale de
// /usr/local/lib/kindling/kling en el daemon (from_host): tiene que ser el del
// propio daemon, o el constructor de dentro y el de fuera no harían lo mismo.
func imagesBuildTools(args []string) error {
	fs := flag.NewFlagSet("image build-tools", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("as", BuildToolsImage, "image name")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	cli := api.NewClient(hostOf(*host))

	fmt.Printf("Building %q: the min base plus the oci builder (kling) inside... ", *name)
	spec, _ := json.Marshal(BaseSpec{})
	if _, err := cli.BuildImage(ctx, api.BuildImageRequest{Name: *name, Base: "min", GrowMB: 256, Builder: "base", Spec: spec}); err != nil {
		fmt.Println("✗")
		return err
	}
	res, err := cli.PutImageFile(ctx, *name, api.PutImageFileRequest{Path: "/usr/local/bin/kling", Mode: "0755",
		FromHost: "kling", Create: true})
	if err == nil && res.Error != "" {
		err = fmt.Errorf("%s", res.Error)
	}
	if err != nil {
		fmt.Println("✗")
		return fmt.Errorf("%w\n(the daemon needs its own binary in its lib dir: sudo install -m755 /usr/local/bin/kling /usr/local/lib/kindling/kling)", err)
	}
	fmt.Println("✓")
	fmt.Printf("Now: kling image import -isolate <ref>\n")
	return nil
}
