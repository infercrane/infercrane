package managedbilling

import (
	"context"

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
	CreateCheckoutSession(context.Context, string, int64) (domain.ManagedCheckoutSession, error)
	ParseWebhook([]byte, string) (domain.ManagedPaymentEvent, error)
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
