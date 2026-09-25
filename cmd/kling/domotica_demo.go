package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/juan52878911/kindling/pkg/domotica"
	"github.com/juan52878911/kindling/pkg/domotica/room"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// kling domotica demo: la habitación de la demo en una página web. Sin daemon
// contestan las capas 1 y 2 (en el proceso) y las demás salen «no disponible»;
// con -von, la capa 4 va por un gateway de IA en el proceso sobre el daemon de
// -H (o por -ai), y solo decide si su evaluación la respalda.
func cmdDomoticaDemo(args []string) error {
	fs := flag.NewFlagSet("domotica demo", flag.ExitOnError)
	vo := vonFlags(fs)
	listen := fs.String("listen", "127.0.0.1:8088", "address of the web page (loopback by default)")
	intentPath := fs.String("intent", "", "intent model (.jev)")
	slotsPath := fs.String("slots", "", "slot model (.jevs)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	d, err := loadDecider(*intentPath, *slotsPath)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	vc, err := vo.connect(ctx)
	if err != nil {
		return err
	}
	defer vc.Close()
	bc := buildCascade(d, vc, *vo.force, *intentPath, *slotsPath)

	layers := []room.LayerInfo{{Name: domotica.LayerTemplate, Status: "on", Detail: fmt.Sprintf("%d demo templates", len(domotica.DemoTemplates))}}
	if d.Intent != nil {
		layers = append(layers, room.LayerInfo{Name: domotica.LayerJEV, Status: "on"})
	} else {
		layers = append(layers, room.LayerInfo{Name: domotica.LayerJEV, Status: "unavailable", Detail: "no intent.jev in " + domoticaModelsDir()})
	}
	layers = append(layers, room.LayerInfo{Name: domotica.LayerEncoder, Status: "unavailable", Detail: "the sentence encoder is not wired yet"})
	switch {
	case vc == nil:
		layers = append(layers, room.LayerInfo{Name: domotica.LayerVON, Status: "unavailable", Detail: "run with -von <golden> and a daemon"})
	case bc.von == "on":
		layers = append(layers, room.LayerInfo{Name: domotica.LayerVON, Status: "on", Detail: bc.note + "; " + vc.Desc})
	case bc.von == "forced":
		layers = append(layers, room.LayerInfo{Name: domotica.LayerVON, Status: "forced", Detail: bc.note})
	default:
		layers = append(layers, room.LayerInfo{Name: domotica.LayerVON, Status: "off", Detail: bc.note})
	}

	opts := room.Options{
		Decide:       bc.cascade.Decide,
		Layers:       layers,
		Presets:      room.Presets,
		LoopbackOnly: scheduler.IsLoopback(*listen),
	}
	if vc != nil {
		opts.ExtraStats = func(ctx context.Context) any {
			st, err := vc.Stats(ctx)
			if err != nil || st == nil {
				return nil
			}
			return st
		}
	}
	srv := room.NewServer(opts)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	go srv.Run(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	fmt.Printf("demo room on http://%s/\n", ln.Addr())
	for _, l := range layers {
		fmt.Printf("  %-8s %-11s %s\n", l.Name, l.Status, l.Detail)
	}
	if !opts.LoopbackOnly {
		fmt.Println("  WARNING: not on loopback and without authentication: anyone who reaches it can send commands (and wake the VON replica)")
	}
	err = hs.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
