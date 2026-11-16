package notification

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

const (
	// MaxAttempts bounds the sends of one event to one destination. With
	// RetryDelay the last attempt is about 1 hour 45 minutes after the first.
	MaxAttempts = 6
	// DefaultInterval is how often the notifier looks for events.
	DefaultInterval = 15 * time.Second

	claimLease      = time.Minute
	claimBatch      = 50
	campaignsPerRun = 200
)

// NotifierStore is the read and queue boundary the background notifier uses.
type NotifierStore interface {
	Store
	NotificationTenants(context.Context) ([]string, error)
	OptimizationCampaignsUpdatedSince(context.Context, string, time.Time, int) ([]domain.OptimizationCampaign, error)
	OptimizationCampaignTransitionsSince(context.Context, string, time.Time) ([]domain.OptimizationCampaignTransition, error)
	NotificationEventKeysSince(context.Context, string, time.Time) (map[string]bool, error)
	ClaimNotificationDeliveries(context.Context, time.Duration, int) ([]domain.NotificationDelivery, error)
}

// Notifier queues campaign events for every enabled destination and sends
// the queued deliveries. Every replica may run it: queuing is idempotent per
// destination and event, and a send holds a lease on its delivery.
type Notifier struct {
	Service  Service
	Store    NotifierStore
	Reports  optimizationreport.Store
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
}

// RetryDelay is the wait after the given failed attempt: 30 seconds, then
// four times longer each time, at most an hour.
func RetryDelay(attempt int) time.Duration {
	delay := 30 * time.Second
	for index := 1; index < attempt && delay < time.Hour; index++ {
		delay *= 4
	}
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func (n Notifier) Run(ctx context.Context) {
	interval := n.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, 4*interval)
		if _, err := n.Queue(pass); err != nil && ctx.Err() == nil {
			n.logger().Warn("notification queue pass failed", "error", err)
		}
		if _, err := n.Send(pass); err != nil && ctx.Err() == nil {
			n.logger().Warn("notification send pass failed", "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Queue derives events for every workspace with an enabled destination and
// queues the ones not queued before. It returns how many it queued.
func (n Notifier) Queue(ctx context.Context) (int, error) {
	if n.Store == nil {
		return 0, errors.New("notifier store is not configured")
	}
	tenants, err := n.Store.NotificationTenants(ctx)
	if err != nil {
		return 0, err
	}
	queued := 0
	var errs []error
	for _, tenant := range tenants {
		count, tenantErr := n.queueTenant(ctx, tenant)
		queued += count
		if tenantErr != nil {
			errs = append(errs, tenantErr)
		}
	}
	return queued, errors.Join(errs...)
}

func (n Notifier) queueTenant(ctx context.Context, tenant string) (int, error) {
	now := n.now()
	since := now.Add(-Lookback)
	subscriptions, err := n.Store.NotificationSubscriptionsForTenant(ctx, tenant)
	if err != nil {
		return 0, err
	}
	enabled := subscriptions[:0]
	for _, subscription := range subscriptions {
		if subscription.Enabled {
			enabled = append(enabled, subscription)
		}
	}
	if len(enabled) == 0 {
		return 0, nil
	}
	campaigns, err := n.Store.OptimizationCampaignsUpdatedSince(ctx, tenant, since, campaignsPerRun)
	if err != nil {
		return 0, err
	}
	transitions, err := n.Store.OptimizationCampaignTransitionsSince(ctx, tenant, since)
	if err != nil {
		return 0, err
	}
	byCampaign := map[string][]domain.OptimizationCampaignTransition{}
	for _, transition := range transitions {
		byCampaign[transition.CampaignID] = append(byCampaign[transition.CampaignID], transition)
	}
	queuedKeys, err := n.Store.NotificationEventKeysSince(ctx, tenant, since)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, campaign := range campaigns {
		var outcome *Outcome
		reportRead := false
		for _, notice := range CampaignNotices(campaign, byCampaign[campaign.ID], now) {
			if notice.OccurredAt.Before(since) {
				continue
			}
			bodies := map[string][]byte{}
			for _, subscription := range enabled {
				if !wants(subscription, notice.Type) || notice.OccurredAt.Before(subscription.CreatedAt) || queuedKeys[subscription.ID+"/"+notice.ID] {
					continue
				}
				if needsReport(notice.Type) && !reportRead {
					reportRead = true
					outcome = n.outcome(ctx, tenant, campaign.ID)
				}
				encoded, ok := bodies[subscription.Destination]
				if !ok {
					body := CampaignBody(tenant, notice, campaign, outcome, RunLinks(n.Service.ConsoleURL, campaign.ID))
					if encoded, err = Encode(subscription.Destination, body); err != nil {
						return queued, err
					}
					bodies[subscription.Destination] = encoded
				}
				_, created, insertErr := n.Store.InsertNotificationDelivery(ctx, domain.NotificationDelivery{TenantID: tenant, SubscriptionID: subscription.ID, EventID: notice.ID, EventType: notice.Type, CampaignID: campaign.ID, Body: string(encoded), BodyDigest: digest(encoded), MaxAttempts: MaxAttempts, OccurredAt: notice.OccurredAt}, 0)
				if errors.Is(insertErr, domain.ErrNotFound) {
					// The destination was deleted during this pass.
					continue
				}
				if insertErr != nil {
					return queued, insertErr
				}
				if created {
					queued++
				}
				queuedKeys[subscription.ID+"/"+notice.ID] = true
			}
		}
	}
	return queued, nil
}

// outcome reads the decision from the outcome report. An unreadable report
// leaves the outcome unknown; the event is still sent.
func (n Notifier) outcome(ctx context.Context, tenant, campaignID string) *Outcome {
	if n.Reports == nil {
		return nil
	}
	report, err := optimizationreport.Load(ctx, n.Reports, tenant, campaignID)
	if err != nil {
		n.logger().Warn("notification outcome report unavailable", "campaign_id", campaignID, "error", err)
		return nil
	}
	outcome := OutcomeFromReport(report)
	return &outcome
}

// Send sends every due delivery once. It returns how many it attempted.
func (n Notifier) Send(ctx context.Context) (int, error) {
	if n.Store == nil {
		return 0, errors.New("notifier store is not configured")
	}
	due, err := n.Store.ClaimNotificationDeliveries(ctx, claimLease, claimBatch)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, delivery := range due {
		if err = n.sendOne(ctx, delivery); err != nil {
			errs = append(errs, err)
		}
	}
	return len(due), errors.Join(errs...)
}

func (n Notifier) sendOne(ctx context.Context, delivery domain.NotificationDelivery) error {
	subscription, err := n.Store.NotificationSubscriptionForTenant(ctx, delivery.TenantID, delivery.SubscriptionID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !subscription.Enabled && delivery.EventType != EventTest {
		_, err = n.Store.RecordNotificationDeliveryAttempt(ctx, delivery.ID, false, 0, "destination_disabled", nil)
		return err
	}
	target, secret, err := n.Service.open(subscription)
	if err != nil {
		_, recordErr := n.Store.RecordNotificationDeliveryAttempt(ctx, delivery.ID, false, 0, "destination_unreadable", nil)
		return errors.Join(err, recordErr)
	}
	retryAt := n.now().Add(RetryDelay(delivery.Attempts + 1))
	result, err := n.Service.attempt(ctx, delivery, target, secret, &retryAt)
	if err != nil {
		return err
	}
	if result.Status == "failed" {
		n.logger().Warn("notification delivery failed", "delivery_id", result.ID, "event", result.EventType, "attempts", result.Attempts, "error_code", result.ErrorCode)
	}
	return nil
}

func wants(subscription domain.NotificationSubscription, event string) bool {
	for _, wanted := range subscription.Events {
		if wanted == event {
			return true
		}
	}
	return false
}

func (n Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now().UTC()
	}
	return time.Now().UTC()
}

func (n Notifier) logger() *slog.Logger {
	if n.Logger != nil {
		return n.Logger
	}
	return slog.Default()
}
