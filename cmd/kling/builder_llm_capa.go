package main

import (
	"errors"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
)

// capaLLM es lo que hace falta para escribir la capa del modelo en Go.
type capaLLM struct {
	Name, BaseImg, Agent, Rootfs, Service, Out string
	T                                          time.Time
	Reflink, Modelo                            string
}

type resultadoCapa struct {
	Stats      ext4.Stats
	Compartido int64
}

func construirCapaLLM(c capaLLM) (resultadoCapa, error) {
	return resultadoCapa{}, errors.New("not implemented")
}
