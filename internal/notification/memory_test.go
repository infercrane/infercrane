package notification

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// memoryStore is an in-memory NotifierStore and optimizationreport.Store with
// the same uniqueness, claim, and attempt rules as the PostgreSQL store.
type memoryStore struct {
	mu            sync.Mutex
	now           func() time.Time
	subscriptions map[string]domain.NotificationSubscription
	deliveries    []domain.NotificationDelivery
	claims        map[string]time.Time
	campaigns     []domain.OptimizationCampaign
	transitions   []domain.OptimizationCampaignTransition
	next          int
}

func newMemoryStore(now func() time.Time) *memoryStore {
	return &memoryStore{now: now, subscriptions: map[string]domain.NotificationSubscription{}, claims: map[string]time.Time{}}
}

func (m *memoryStore) id(prefix string) string {
	m.next++
	return fmt.Sprintf("%s-%d", prefix, m.next)
}

func (m *memoryStore) CreateNotificationSubscription(_ context.Context, tenant string, item domain.NotificationSubscription) (domain.NotificationSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item.TenantID, item.CreatedAt, item.UpdatedAt = tenant, m.now(), m.now()
	m.subscriptions[item.ID] = item
	return item, nil
}

func (m *memoryStore) UpdateNotificationSubscription(_ context.Context, tenant string, item domain.NotificationSubscription) (domain.NotificationSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.subscriptions[item.ID]
	if !ok || current.TenantID != tenant {
		return domain.NotificationSubscription{}, domain.ErrNotFound
	}
	item.UpdatedAt = m.now()
	m.subscriptions[item.ID] = item
	return item, nil
}

func (m *memoryStore) DeleteNotificationSubscription(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.subscriptions[id]
	if !ok || current.TenantID != tenant {
		return domain.ErrNotFound
	}
	delete(m.subscriptions, id)
	kept := m.deliveries[:0]
	for _, delivery := range m.deliveries {
		if delivery.SubscriptionID != id {
			kept = append(kept, delivery)
		}
	}
	m.deliveries = kept
	return nil
}

func (m *memoryStore) NotificationSubscriptionForTenant(_ context.Context, tenant, id string) (domain.NotificationSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.subscriptions[id]
	if !ok || item.TenantID != tenant {
		return domain.NotificationSubscription{}, domain.ErrNotFound
	}
	return item, nil
}

func (m *memoryStore) NotificationSubscriptionsForTenant(_ context.Context, tenant string) ([]domain.NotificationSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := []domain.NotificationSubscription{}
	for _, item := range m.subscriptions {
		if item.TenantID == tenant {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (m *memoryStore) NotificationTenants(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	tenants := []string{}
	for _, item := range m.subscriptions {
		if item.Enabled && !seen[item.TenantID] {
			seen[item.TenantID] = true
			tenants = append(tenants, item.TenantID)
		}
	}
	sort.Strings(tenants)
	return tenants, nil
}

func (m *memoryStore) InsertNotificationDelivery(_ context.Context, item domain.NotificationDelivery, claim time.Duration) (domain.NotificationDelivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subscriptions[item.SubscriptionID]; !ok {
		return domain.NotificationDelivery{}, false, domain.ErrNotFound
	}
	for _, existing := range m.deliveries {
		if existing.SubscriptionID == item.SubscriptionID && existing.EventID == item.EventID {
			return existing, false, nil
		}
	}
	item.ID, item.Status, item.NextAttemptAt, item.CreatedAt, item.UpdatedAt = m.id("delivery"), "pending", m.now(), m.now(), m.now()
	if claim > 0 {
		m.claims[item.ID] = m.now().Add(claim)
	}
	m.deliveries = append(m.deliveries, item)
	return item, true, nil
}

func (m *memoryStore) ClaimNotificationDeliveries(_ context.Context, lease time.Duration, limit int) ([]domain.NotificationDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	claimed := []domain.NotificationDelivery{}
	for _, delivery := range m.deliveries {
		if len(claimed) == limit {
			break
		}
		if until, held := m.claims[delivery.ID]; delivery.Status != "pending" || delivery.NextAttemptAt.After(now) || held && until.After(now) {
			continue
		}
		m.claims[delivery.ID] = now.Add(lease)
		claimed = append(claimed, delivery)
	}
	return claimed, nil
}

func (m *memoryStore) RecordNotificationDeliveryAttempt(_ context.Context, id string, delivered bool, status int, code string, retryAt *time.Time) (domain.NotificationDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index := range m.deliveries {
		row := &m.deliveries[index]
		if row.ID != id || row.Status != "pending" {
			continue
		}
		row.Attempts++
		row.ResponseStatus, row.ErrorCode, row.UpdatedAt = status, code, m.now()
		switch {
		case delivered:
			row.Status = "delivered"
			at := m.now()
			row.DeliveredAt = &at
		case retryAt != nil && row.Attempts < row.MaxAttempts:
			row.NextAttemptAt = *retryAt
		default:
			row.Status = "failed"
		}
		delete(m.claims, id)
		return *row, nil
	}
	return domain.NotificationDelivery{}, domain.ErrConflict
}

func (m *memoryStore) OptimizationCampaignsUpdatedSince(_ context.Context, tenant string, since time.Time, _ int) ([]domain.OptimizationCampaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.OptimizationCampaign{}
	for _, campaign := range m.campaigns {
		if campaign.TenantID == tenant && !campaign.UpdatedAt.Before(since) {
			out = append(out, campaign)
		}
	}
	return out, nil
}

func (m *memoryStore) OptimizationCampaignTransitionsSince(_ context.Context, tenant string, since time.Time) ([]domain.OptimizationCampaignTransition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.OptimizationCampaignTransition{}
	for _, transition := range m.transitions {
		if transition.TenantID == tenant && !transition.CreatedAt.Before(since) {
			out = append(out, transition)
		}
	}
	return out, nil
}

func (m *memoryStore) NotificationEventKeysSince(_ context.Context, tenant string, since time.Time) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := map[string]bool{}
	for _, delivery := range m.deliveries {
		if delivery.TenantID == tenant && !delivery.OccurredAt.Before(since) {
			keys[delivery.SubscriptionID+"/"+delivery.EventID] = true
		}
	}
	return keys, nil
}

func (m *memoryStore) setCampaign(campaign domain.OptimizationCampaign) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index := range m.campaigns {
		if m.campaigns[index].ID == campaign.ID {
			m.campaigns[index] = campaign
			return
		}
	}
	m.campaigns = append(m.campaigns, campaign)
}

func (m *memoryStore) addTransition(transition domain.OptimizationCampaignTransition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitions = append(m.transitions, transition)
}

func (m *memoryStore) delivered(eventType string) []domain.NotificationDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.NotificationDelivery{}
	for _, delivery := range m.deliveries {
		if delivery.EventType == eventType {
			out = append(out, delivery)
		}
	}
	return out
}

// optimizationreport.Store: the campaign exists; no evidence rows do.

func (m *memoryStore) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, campaign := range m.campaigns {
		if campaign.TenantID == tenant && campaign.ID == id {
			return campaign, nil
		}
	}
	return domain.OptimizationCampaign{}, domain.ErrNotFound
}

func (m *memoryStore) BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error) {
	return nil, nil
}

func (m *memoryStore) QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error) {
	return nil, nil
}

func (m *memoryStore) ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error) {
	return nil, nil
}

func (m *memoryStore) ReplayTraceByDigest(context.Context, string, string) (domain.ReplayTrace, error) {
	return domain.ReplayTrace{}, domain.ErrNotFound
}
