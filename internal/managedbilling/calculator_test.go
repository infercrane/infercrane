package managedbilling

import "testing"

func TestTokenCostUsesIntegerMicrodollarsAndRoundsEachDimensionUp(t *testing.T) {
	got, err := TokenCostMicrousd(1, 1, 100_000, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("cost=%d want=2", got)
	}
	got, err = TokenCostMicrousd(1_000_000, 250_000, 100_000, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 200_000 {
		t.Fatalf("cost=%d want=200000", got)
	}
}

func TestTokenCostRejectsNegativeAndOverflow(t *testing.T) {
	if _, err := TokenCostMicrousd(-1, 0, 1, 1); err == nil {
		t.Fatal("expected negative token rejection")
	}
	if _, err := TokenCostMicrousd(int(^uint(0)>>1), 0, int64(^uint64(0)>>1), 0); err == nil {
		t.Fatal("expected overflow rejection")
	}
}

func TestMinimumRetailPriceEnforcesGrossMarginAndRoundsUp(t *testing.T) {
	got, err := MinimumRetailPriceMicrousd(80_000, 2_000)
	if err != nil || got != 100_000 {
		t.Fatalf("minimum retail=%d err=%v want=100000", got, err)
	}
	got, err = MinimumRetailPriceMicrousd(1, 1_500)
	if err != nil || got != 2 {
		t.Fatalf("rounded minimum retail=%d err=%v want=2", got, err)
	}
	if _, err = MinimumRetailPriceMicrousd(1, 10_000); err == nil {
		t.Fatal("expected impossible margin rejection")
	}
}
