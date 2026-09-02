package managedbilling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/infercrane/infercrane/internal/domain"
)

const MicrousdPerUSCent int64 = 10_000

var checkoutAmounts = [...]int64{
	25_000_000,
	50_000_000,
	100_000_000,
	250_000_000,
	500_000_000,
}

type CheckoutProvider interface {
	CreateCheckoutSession(context.Context, string, string, int64) (domain.ManagedCheckoutSession, error)
	ParseWebhook([]byte, string) (domain.ManagedPaymentEvent, error)
}

// FundingIntentID is stable for one tenant and caller-supplied idempotency
// key. Reusing the key with different parameters is rejected by the store.
func FundingIntentID(tenant, idempotencyKey string) string {
	digest := sha256.Sum256([]byte(tenant + "\x00" + idempotencyKey))
	return "funding_" + hex.EncodeToString(digest[:16])
}

func CheckoutAmounts() []int64 {
	return append([]int64(nil), checkoutAmounts[:]...)
}

func ValidateCheckoutAmount(amount int64) bool {
	for _, allowed := range checkoutAmounts {
		if amount == allowed {
			return true
		}
	}
	return false
}
