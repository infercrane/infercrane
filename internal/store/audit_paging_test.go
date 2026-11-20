package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// Several events can share a timestamp. Paging by (created_at, id) returns
// each exactly once even when a page boundary falls inside such a tie; the
// old created_at-only cursor skipped the rest of the tie.
func TestAuditEventsPageEveryEventWhenTimestampsTie(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	const tenant = "audit-paging"
	if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
		t.Fatal(err)
	}
	tie := time.Date(2026, 11, 20, 12, 0, 0, 123456000, time.UTC)
	insert := func(id string, at time.Time) {
		t.Helper()
		if _, err := s.ExecContext(ctx, `INSERT INTO audit_events(id,tenant_id,actor,action,resource_type,resource_name,outcome,payload_json,created_at) VALUES(?,?,'tester','test.event','test',?,'succeeded','{}'::jsonb,?)`, id, tenant, id, at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 5 {
		insert(fmt.Sprintf("event-%02d", index), tie)
	}
	insert("event-05", tie.Add(-time.Second))
	insert("event-06", tie.Add(-time.Second))
	insert("event-07", tie.Add(time.Second))

	var cursor domain.AuditCursor
	var seen []string
	for range 10 {
		events, err := s.AuditEventsForTenant(ctx, tenant, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			seen = append(seen, event.ID)
		}
		if len(events) < 2 {
			break
		}
		// The cursor travels as RFC3339 text, as it does through the API.
		last := events[len(events)-1]
		at, err := time.Parse(time.RFC3339Nano, last.CreatedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		cursor = domain.AuditCursor{CreatedAt: at, ID: last.ID}
	}
	if want := []string{"event-07", "event-04", "event-03", "event-02", "event-01", "event-00", "event-06", "event-05"}; !slices.Equal(seen, want) {
		t.Fatalf("paged events = %v, want %v", seen, want)
	}

	// before alone keeps its meaning: every event older than that time.
	older, err := s.AuditEventsForTenant(ctx, tenant, domain.AuditCursor{CreatedAt: tie}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ids := auditIDs(older); !slices.Equal(ids, []string{"event-06", "event-05"}) {
		t.Fatalf("events before the tie = %v", ids)
	}
}

func TestAuditEventsClampTheLimitToTheMaximum(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	const tenant = "audit-limit"
	if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecContext(ctx, `INSERT INTO audit_events(id,tenant_id,actor,action,resource_type,resource_name,outcome,payload_json,created_at) SELECT 'event-'||n,?,'tester','test.event','test','x','succeeded','{}'::jsonb,NOW()-n*INTERVAL '1 millisecond' FROM generate_series(1,?) AS n`, tenant, MaxAuditPageSize+20); err != nil {
		t.Fatal(err)
	}
	for limit, want := range map[int]int{1000: MaxAuditPageSize, 0: DefaultAuditPageSize, -3: DefaultAuditPageSize, 7: 7} {
		events, err := s.AuditEventsForTenant(ctx, tenant, domain.AuditCursor{}, limit)
		if err != nil || len(events) != want {
			t.Fatalf("limit %d returned %d events, want %d (err=%v)", limit, len(events), want, err)
		}
	}
}

func auditIDs(events []domain.AuditEvent) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}
