// Package pricing defines timestamped provider-price estimates.
package pricing

import (
	"context"
	"errors"
	"time"
)

var ErrUnavailable = errors.New("pricing unavailable")

type Request struct {
	Cloud, Region, GPU string
	Replicas           int
}
type Estimate struct {
	Currency, Source string
	Hourly           float64
	ObservedAt       time.Time
	StaleAfter       time.Duration
}

func (e Estimate) Stale(now time.Time) bool {
	return e.ObservedAt.IsZero() || e.StaleAfter <= 0 || now.Sub(e.ObservedAt) > e.StaleAfter
}

type Provider interface {
	Estimate(context.Context, Request) (Estimate, error)
}

type Catalog struct{ Prices map[Request]Estimate }

func (c Catalog) Estimate(_ context.Context, request Request) (Estimate, error) {
	estimate, ok := c.Prices[request]
	if !ok {
		return Estimate{}, ErrUnavailable
	}
	return estimate, nil
}
