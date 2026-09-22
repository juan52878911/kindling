package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/juan52878911/kindling/pkg/api"
)

// imagesBuild construye una imagen con un constructor instalado en el daemon.
//
//	kling images build <name> -builder <b> [-spec spec.json|-] [-base B] [-grow MiB]
func imagesBuild(args []string) error {
	fs := flag.NewFlagSet("images build", flag.ExitOnError)
	host := hostFlag(fs)
	builder := fs.String("builder", "", "builder installed on the daemon (/usr/local/lib/kindling/builders/<name>)")
	specFile := fs.String("spec", "", "JSON file with the builder's spec ('-' = stdin)")
	base := fs.String("base", "", "base image")
	grow := fs.Int("grow", 0, "MiB to reserve for the layer")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *builder == "" {
		return fmt.Errorf("usage: kling images build <name> -builder <builder> [-spec spec.json|-] [-base B] [-grow MiB]")
	}
	req := api.BuildImageRequest{Name: fs.Arg(0), Builder: *builder, Base: *base, GrowMB: *grow}
	if *specFile != "" {
		var b []byte
		var err error
		if *specFile == "-" {
			b, err = api.LeerCuerpo(os.Stdin, 8<<20)
		} else {
			b, err = os.ReadFile(*specFile)
		}
		if err != nil {
			return err
		}
		if !json.Valid(b) {
			return fmt.Errorf("%s is not valid JSON", *specFile)
		}
		req.Spec = b
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	fmt.Printf("building %s with builder %s (can take minutes)...\n", req.Name, req.Builder)
	res, err := api.NewClient(hostOf(*host)).BuildImage(ctx, req)
	if res != nil && res.Output != "" {
		fmt.Print(res.Output)
	}
	if err != nil {
		return err
	}
	fmt.Printf("image %s ready: %s\n", res.Name, res.Path)
	return nil
}

// imagesCat imprime un fichero de dentro de una imagen.
func imagesCat(args []string) error {
	fs := flag.NewFlagSet("images cat", flag.ExitOnError)
	host := hostFlag(fs)
	stat := fs.Bool("stat", false, "only say whether it is there, its size and sha256")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: kling images cat <image> </path/inside> [-stat]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	if *stat {
		st, err := c.StatImageFile(ctx, fs.Arg(0), fs.Arg(1))
		if err != nil {
			return err
		}
		if !st.Exists {
			fmt.Printf("%s: not in %s\n", st.Path, fs.Arg(0))
			return nil
		}
		fmt.Printf("%s: %d bytes · sha256 %s\n", st.Path, st.Size, st.SHA256)
		return nil
	}
	b, err := c.ImageFile(ctx, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// imagesPut mete un fichero en una imagen ya construida.
func imagesPut(args []string) error {
	fs := flag.NewFlagSet("images put", flag.ExitOnError)
	host := hostFlag(fs)
	file := fs.String("file", "", "local file to upload (up to 8 MiB)")
	fromHost := fs.String("from-host", "", "file in the daemon's /usr/local/lib/kindling instead of a local one")
	mode := fs.String("mode", "0644", "permissions inside the image")
	create := fs.Bool("create", false, "create it if the image doesn't have it (default: only replace)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 2 || (*file == "") == (*fromHost == "") {
		return fmt.Errorf("usage: kling images put <image> </path/inside> (-file local | -from-host name) [-mode 0755] [-create]")
	}
	req := api.PutImageFileRequest{Path: fs.Arg(1), Mode: *mode, FromHost: *fromHost, Create: *create}
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		req.ContentB64 = base64.StdEncoding.EncodeToString(b)
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	res, err := api.NewClient(hostOf(*host)).PutImageFile(ctx, fs.Arg(0), req)
	if err != nil {
		return err
	}
	switch {
	case res.Error != "":
		return fmt.Errorf("%s", res.Error)
	case res.Updated:
		fmt.Printf("%s: %s updated\n", res.Image, res.Path)
	default:
		fmt.Printf("%s: %s already identical\n", res.Image, res.Path)
	}
	return nil
}
