// Package authn maintains a database-independent credential snapshot for request paths.
package authn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type Source interface {
	ActiveCredentials(context.Context) ([]domain.CredentialRecord, error)
}
type Cache struct {
	Source   Source
	Interval time.Duration
	snapshot atomic.Pointer[map[string]domain.Principal]
}

func (c *Cache) Refresh(ctx context.Context) error {
	records, err := c.Source.ActiveCredentials(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]domain.Principal, len(records))
	for _, record := range records {
		next[record.Hash] = record.Principal
	}
	c.snapshot.Store(&next)
	return nil
}
func (c *Cache) AuthenticatePrincipal(_ context.Context, token string) (domain.Principal, error) {
	current := c.snapshot.Load()
	if current == nil {
		return domain.Principal{}, domain.ErrNotFound
	}
	sum := sha256.Sum256([]byte(token))
	principal, ok := (*current)[hex.EncodeToString(sum[:])]
	if !ok {
		return domain.Principal{}, domain.ErrNotFound
	}
	return principal, nil
}
func (c *Cache) Run(ctx context.Context) error {
	if c.Source == nil {
		return errors.New("credential source is required")
	}
	interval := c.Interval
	if interval <= 0 {
		interval = time.Second
	}
	if err := c.Refresh(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = c.Refresh(ctx)
		}
	}
}
