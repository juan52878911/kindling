package scheduler

import "context"

// FillSpares, DrainSpares y Retire: declaraciones (ronda 9, Z4).
func (g *Scheduler) FillSpares(ctx context.Context, service string) {}
func (g *Scheduler) DrainSpares(ctx context.Context)                 {}
func (g *Scheduler) Retire(ctx context.Context, service string)      {}
