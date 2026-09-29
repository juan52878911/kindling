package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// graphCmd simula `kling graph up|inspect|rm`. Se llama con el mutex de Run ya
// tomado.
func (f *fakeKling) graphCmd(args []string, stdin string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("fake graph: no subcommand")
	}
	if f.graphs == nil {
		f.graphs, f.graphSecrets = map[string]*api.Graph{}, map[string]string{}
	}
	switch args[0] {
	case "up":
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return nil, err
		}
		f.graphFiles = append(f.graphFiles, string(raw))
		if f.graphUpFails {
			return nil, errors.New("graph up: 507 not enough memory")
		}
		var g api.Graph
		if err := json.Unmarshal(raw, &g); err != nil {
			return nil, err
		}
		if f.graphs[g.Name] != nil {
			return nil, errors.New("graph up: 409 already exists")
		}
		g.ID = fmt.Sprintf("%016x", 0x6a0000+len(f.graphs)+1)
		for n, nd := range g.Nodes {
			labels := clone(nd.Labels)
			labels[api.LabelGraph], labels[api.LabelGraphNode] = g.ID, n
			if len(nd.Ports) > 0 {
				labels[api.LabelPorts] = strconv.Itoa(nd.Ports[0])
			}
			mc := f.newMachine(g.Name+"-"+n, labels)
			mc.From = nd.From
			nd.MachineID, nd.State = mc.ID, "running"
			g.Nodes[n] = nd
		}
		f.graphs[g.Name] = &g
		f.graphSecrets[g.Name] = strings.TrimRight(stdin, "\n")
		return json.Marshal(&g)
	case "inspect":
		g := f.graphs[args[1]]
		if g == nil {
			return nil, errors.New("graph \"" + args[1] + "\" not found")
		}
		return json.Marshal(g)
	case "rm":
		g := f.graphs[args[1]]
		if g == nil {
			return nil, errors.New("graph \"" + args[1] + "\" not found")
		}
		for _, nd := range g.Nodes {
			delete(f.machines, nd.MachineID)
			delete(f.verifier, nd.MachineID)
		}
		delete(f.graphs, args[1])
		return nil, nil
	}
	return nil, fmt.Errorf("fake graph: unexpected %v", args)
}
