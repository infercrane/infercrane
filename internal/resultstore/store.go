package resultstore

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// ErrNotFound is returned for a record that is not in the caller's tenant.
var ErrNotFound = errors.New("result record not found")

// Limits on one read and one write.
const (
	DefaultListLimit = 200
	MaxListLimit     = 2000
	MaxPutRecords    = 2000
)

// Store keeps records per tenant. The control plane's PostgreSQL store and
// Local implement it.
type Store interface {
	// PutResultRecords adds sealed records to the tenant; a record whose ID
	// is already there is skipped. It returns how many were added.
	PutResultRecords(ctx context.Context, tenant string, records []Record) (int, error)
	// ListResultRecords reads the tenant's own records, newest first.
	ListResultRecords(ctx context.Context, tenant string, filter Filter) ([]Record, error)
	// GetResultRecord reads one of the tenant's own records.
	GetResultRecord(ctx context.Context, tenant, id string) (Record, error)
	// SharedResultRecords reads other tenants' records whose owners set
	// SharingNumbers, each reduced by NumbersOnly. Only the prior calls it;
	// its output is never returned by a read.
	SharedResultRecords(ctx context.Context, tenant string, filter Filter) ([]Record, error)
}

// Filter narrows a read. Empty fields match everything.
type Filter struct {
	Model       string `json:"model,omitempty"`
	Accelerator string `json:"accelerator,omitempty"`
	Engine      string `json:"engine,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Metric      string `json:"metric,omitempty"`
	Technique   string `json:"technique,omitempty"`
	Cell        string `json:"cell,omitempty"`
	MinLoad     int    `json:"min_load,omitempty"`
	MaxLoad     int    `json:"max_load,omitempty"`
	// Limit is DefaultListLimit when 0 and at most MaxListLimit.
	Limit int `json:"limit,omitempty"`
}

// Bounded returns the filter's limit, defaulted and capped.
func (f Filter) Bounded() int {
	switch {
	case f.Limit <= 0:
		return DefaultListLimit
	case f.Limit > MaxListLimit:
		return MaxListLimit
	}
	return f.Limit
}

// Match reports whether a record passes the filter.
func (f Filter) Match(r Record) bool {
	equal := func(want, got string) bool { return want == "" || strings.EqualFold(want, got) }
	return equal(f.Model, r.Model.ID) && equal(f.Accelerator, r.Accelerator()) && equal(f.Engine, r.Result.Software.Engine) &&
		equal(f.Kind, r.Kind()) && equal(f.Metric, r.Metric()) && equal(f.Technique, r.Result.Technique) && equal(f.Cell, r.Cell.ID) &&
		(f.MinLoad <= 0 || r.Load() >= f.MinLoad) && (f.MaxLoad <= 0 || r.Load() <= f.MaxLoad)
}

// Newest orders records newest first, then by ID, and keeps the filter's
// limit.
func Newest(records []Record, filter Filter) []Record {
	out := make([]Record, 0, len(records))
	for _, record := range records {
		if filter.Match(record) {
			out = append(out, record)
		}
	}
	slices.SortFunc(out, func(a, b Record) int {
		if c := b.RecordedAt.Compare(a.RecordedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if limit := filter.Bounded(); len(out) > limit {
		out = out[:limit]
	}
	return out
}
