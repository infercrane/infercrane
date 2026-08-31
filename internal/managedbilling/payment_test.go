package managedbilling

import "testing"

func TestCheckoutAmountsAreBoundedAndDefensivelyCopied(t *testing.T) {
	amounts := CheckoutAmounts()
	if len(amounts) != 5 || !ValidateCheckoutAmount(25_000_000) || !ValidateCheckoutAmount(500_000_000) || ValidateCheckoutAmount(25_010_000) || ValidateCheckoutAmount(1_000_000_000) {
		t.Fatalf("unexpected allowed checkout amounts: %v", amounts)
	}
	amounts[0] = 1
	if !ValidateCheckoutAmount(25_000_000) {
		t.Fatal("caller mutated the checkout amount authority")
	}
}
