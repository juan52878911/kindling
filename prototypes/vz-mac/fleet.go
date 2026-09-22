package main

// fleet: pruebas de carga y estrés sobre una flota de microVMs.
//
//	vzproto fleet -n 8 -gate 4 -layer guest/files.layer.ext4 -ip 192.168.64.100 \
//	    -call list_directory -args '{"path":"/tmp"}' -duration 30s -concurrency 16 -cycles 5
//
// Fases, en este orden:
//
//  1. Arranque en frío de N máquinas en paralelo, como mucho `gate` a la vez
//     (el equivalente de KLING_MAX_PARALLEL_BOOT). Cada una con su IP.
//  2. initialize en cada una; se guarda su Mcp-Session-Id.
//  3. Carga: C clientes durante D lanzan tools/call (o tools/list) repartidos
//     por las N máquinas. Latencias p50/p95/p99, req/s, errores.
//  4. Ciclos: K veces por máquina, Pause -> Resume -> petición. Mide la latencia
//     de despertar y si la memoria deriva.
//  5. Flota pausada: memoria con todas en pausa; reanudar todas y una petición.
//  6. Stop de todas.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Code-Hex/vz/v3"
)

type fleetVM struct {
	m       *machine
	session string
	idx     int
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return s[i]
}

func cmdFleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ExitOnError)
	o := commonFlags(fs)
	n := fs.Int("n", 4, "máquinas")
	gate := fs.Int("gate", 4, "arranques simultáneos como mucho")
	call := fs.String("call", "", "herramienta a invocar en la fase de carga (vacío = tools/list)")
	callArgs := fs.String("args", "{}", "argumentos JSON de la herramienta")
	duration := fs.Duration("duration", 20*time.Second, "duración de la fase de carga")
	concurrency := fs.Int("concurrency", 8, "clientes concurrentes")
	cycles := fs.Int("cycles", 3, "ciclos pausa/reanuda por máquina tras la carga")
	idle := fs.Duration("idle", 2*time.Second, "reposo antes de medir memoria")
	jsonOut := fs.String("json", "", "resultados a fichero")
	fs.Parse(args)
	if o.ip == "" || o.layer == "" {
		return fmt.Errorf("fleet necesita -ip (base) y -layer")
	}

	record("baseline (proceso vacío)", 0, "", 0)
	physBase, _ := mem()

	// ---- 1+2: arranque con compuerta e initialize
	vms := make([]*fleetVM, *n)
	var (
		mu        sync.Mutex
		bootLat   []time.Duration
		initLat   []time.Duration
		wg        sync.WaitGroup
		sem       = make(chan struct{}, *gate)
		firstErr  error
		bootStart = time.Now()
	)
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			m, err := o.newMachine(i)
			if err == nil {
				err = m.vm.Start()
			}
			if err == nil {
				err = m.waitState(vz.VirtualMachineStateRunning, 15*time.Second)
			}
			var dBoot time.Duration
			if err == nil {
				_, err = waitHealthz(m.ip, 90*time.Second)
				dBoot = time.Since(t0)
			}
			var dInit time.Duration
			var sid string
			if err == nil {
				dInit, _, sid, err = mcpPost(m.ip, initializeBody, "")
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("máquina #%d: %w", i, err)
				}
				return
			}
			bootLat = append(bootLat, dBoot)
			initLat = append(initLat, dInit)
			vms[i] = &fleetVM{m: m, session: sid, idx: i}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	record(fmt.Sprintf("%d máquinas arrancadas e inicializadas (gate %d)", *n, *gate), time.Since(bootStart), "", 0)
	record("  arranque -> puente escuchando", pct(bootLat, 0.5), fmt.Sprintf("p50; p95 %.0f ms; máx %.0f ms", ms(pct(bootLat, 0.95)), ms(pct(bootLat, 1))), 0)
	record("  initialize (node/python en frío)", pct(initLat, 0.5), fmt.Sprintf("p50; p95 %.0f ms; máx %.0f ms", ms(pct(initLat, 0.95)), ms(pct(initLat, 1))), 0)
	time.Sleep(*idle)
	phys, _ := mem()
	record(fmt.Sprintf("flota en marcha tras %s", *idle), 0, fmt.Sprintf("%.1f MiB por máquina sobre baseline", (phys-physBase)/float64(*n)), 0)

	// ---- 3: carga
	body := toolsListBody
	what := "tools/list"
	if *call != "" {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(*callArgs), &raw); err != nil {
			return fmt.Errorf("-args no es JSON: %w", err)
		}
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "ID", "method": "tools/call",
			"params": map[string]any{"name": *call, "arguments": raw},
		})
		body = string(b)
		what = "tools/call " + *call
	}
	// Cada petición en vuelo necesita un id JSON-RPC distinto: el puente rechaza
	// con 502 los repetidos ("a request with id N is already in flight").
	var nextID atomic.Int64
	withID := func() string {
		id := nextID.Add(1)
		if strings.Contains(body, `"id":"ID"`) {
			return strings.Replace(body, `"id":"ID"`, fmt.Sprintf(`"id":%d`, id), 1)
		}
		return strings.Replace(body, `"id":2`, fmt.Sprintf(`"id":%d`, id), 1)
	}
	var sampleOnce sync.Once
	var (
		lat     []time.Duration
		latMu   sync.Mutex
		okN     atomic.Int64
		errN    atomic.Int64
		rpcErrN atomic.Int64
		lastErr atomic.Value
	)
	deadline := time.Now().Add(*duration)
	var lwg sync.WaitGroup
	for c := 0; c < *concurrency; c++ {
		lwg.Add(1)
		go func(c int) {
			defer lwg.Done()
			k := c
			for time.Now().Before(deadline) {
				v := vms[k%*n]
				k += *concurrency
				d, note, sample, err := mcpPostSample(v.m.ip, withID(), v.session)
				if err == nil && strings.HasPrefix(note, "http 200") {
					sampleOnce.Do(func() { fmt.Printf("    respuesta de muestra: %s\n", sample) })
				}
				if err != nil {
					errN.Add(1)
					lastErr.Store(err.Error())
					continue
				}
				if !strings.HasPrefix(note, "http 200") {
					errN.Add(1)
					lastErr.Store(note)
					continue
				}
				if strings.Contains(note, "rpc-error") {
					rpcErrN.Add(1)
					lastErr.Store(note)
					continue
				}
				okN.Add(1)
				latMu.Lock()
				lat = append(lat, d)
				latMu.Unlock()
			}
		}(c)
	}
	// muestra de memoria a mitad de la carga
	time.Sleep(*duration / 2)
	physMid, _ := mem()
	lwg.Wait()
	total := okN.Load() + errN.Load() + rpcErrN.Load()
	note := fmt.Sprintf("%d ok, %d errores http, %d errores rpc · %.0f req/s · p50 %.1f ms · p95 %.1f ms · p99 %.1f ms · máx %.0f ms",
		okN.Load(), errN.Load(), rpcErrN.Load(), float64(total)/duration.Seconds(),
		ms(pct(lat, 0.5)), ms(pct(lat, 0.95)), ms(pct(lat, 0.99)), ms(pct(lat, 1)))
	if e := lastErr.Load(); e != nil {
		note += " · último error: " + fmt.Sprint(e)
	}
	record(fmt.Sprintf("carga %s, %d clientes, %s", what, *concurrency, *duration), *duration, note, 0)
	record("  memoria a mitad de la carga", 0, fmt.Sprintf("%.1f MiB por máquina sobre baseline", (physMid-physBase)/float64(*n)), 0)
	time.Sleep(*idle)
	physAfter, _ := mem()
	record("  memoria tras la carga", 0, fmt.Sprintf("%.1f MiB por máquina sobre baseline (deriva %.1f)", (physAfter-physBase)/float64(*n), (physAfter-phys)/float64(*n)), 0)

	// ---- 4: ciclos pausa/reanuda
	if *cycles > 0 {
		var wake []time.Duration
		var cwg sync.WaitGroup
		var cmu sync.Mutex
		var cerr error
		for _, v := range vms {
			cwg.Add(1)
			go func(v *fleetVM) {
				defer cwg.Done()
				for k := 0; k < *cycles; k++ {
					if err := v.m.vm.Pause(); err != nil {
						cmu.Lock()
						cerr = err
						cmu.Unlock()
						return
					}
					if err := v.m.waitState(vz.VirtualMachineStatePaused, 10*time.Second); err != nil {
						cmu.Lock()
						cerr = err
						cmu.Unlock()
						return
					}
					time.Sleep(200 * time.Millisecond)
					t0 := time.Now()
					if err := v.m.vm.Resume(); err != nil {
						cmu.Lock()
						cerr = err
						cmu.Unlock()
						return
					}
					if err := v.m.waitState(vz.VirtualMachineStateRunning, 10*time.Second); err != nil {
						cmu.Lock()
						cerr = err
						cmu.Unlock()
						return
					}
					_, note, _, err := mcpPostBody(v.m.ip, withID(), v.session)
					if err != nil || !strings.HasPrefix(note, "http 200") {
						cmu.Lock()
						cerr = fmt.Errorf("#%d ciclo %d: %v %s", v.idx, k, err, note)
						cmu.Unlock()
						return
					}
					d := time.Since(t0)
					cmu.Lock()
					wake = append(wake, d)
					cmu.Unlock()
				}
			}(v)
		}
		cwg.Wait()
		if cerr != nil {
			return fmt.Errorf("ciclos pausa/reanuda: %w", cerr)
		}
		record(fmt.Sprintf("%d ciclos pausa->reanuda->petición por máquina", *cycles), pct(wake, 0.5),
			fmt.Sprintf("p50; p95 %.1f ms; máx %.1f ms (%d despertares)", ms(pct(wake, 0.95)), ms(pct(wake, 1)), len(wake)), 0)
		time.Sleep(*idle)
		physC, _ := mem()
		record("  memoria tras los ciclos", 0, fmt.Sprintf("%.1f MiB por máquina sobre baseline (deriva %.1f)", (physC-physBase)/float64(*n), (physC-physAfter)/float64(*n)), 0)
	}

	// ---- 5: flota pausada
	for _, v := range vms {
		if err := v.m.vm.Pause(); err != nil {
			return err
		}
	}
	for _, v := range vms {
		if err := v.m.waitState(vz.VirtualMachineStatePaused, 10*time.Second); err != nil {
			return err
		}
	}
	time.Sleep(*idle)
	physP, _ := mem()
	record("flota pausada", 0, fmt.Sprintf("%.1f MiB por máquina sobre baseline", (physP-physBase)/float64(*n)), 0)
	t0 := time.Now()
	for _, v := range vms {
		if err := v.m.vm.Resume(); err != nil {
			return err
		}
	}
	for _, v := range vms {
		if err := v.m.waitState(vz.VirtualMachineStateRunning, 10*time.Second); err != nil {
			return err
		}
		if _, note, _, err := mcpPostBody(v.m.ip, withID(), v.session); err != nil || !strings.HasPrefix(note, "http 200") {
			return fmt.Errorf("#%d no responde tras reanudar la flota: %v %s", v.idx, err, note)
		}
	}
	record(fmt.Sprintf("reanudar la flota entera + 1 petición a cada una (%d)", *n), time.Since(t0), "", 0)

	// ---- 6: stop
	t0 = time.Now()
	for _, v := range vms {
		if err := v.m.vm.Stop(); err != nil {
			return err
		}
	}
	record(fmt.Sprintf("Stop() x%d", *n), time.Since(t0), "", 0)
	return dumpJSON(*jsonOut)
}

// mcpPostBody es mcpPost pero marca en la nota si la respuesta JSON-RPC trae
// "error": un 200 con error dentro no es una petición servida.
func mcpPostBody(ip, body, session string) (time.Duration, string, string, error) {
	d, note, sid, err := mcpPostRaw(ip, body, session)
	return d, note, sid, err
}

// mcpPostSample devuelve además un recorte del cuerpo, para enseñar UNA
// respuesta real y comprobar que la herramienta se ejecutó de verdad.
func mcpPostSample(ip, body, session string) (time.Duration, string, string, error) {
	d, note, sample, err := mcpPostRawSample(ip, body, session)
	return d, note, sample, err
}

// dumpJSON con fichero vacío no hace nada; se reutiliza el de main.go.
var _ = os.Getpid
