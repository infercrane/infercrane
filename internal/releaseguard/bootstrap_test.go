package releaseguard

import (
	"reflect"
	"testing"
)

func TestPairedBootstrapIsDeterministicAndClassifiesIntervals(t *testing.T) {
	active := []float64{.90, .88, .92, .86, .91, .89, .93, .87, .90, .92}
	good := []float64{.91, .89, .93, .87, .92, .90, .94, .88, .91, .93}
	one, err := PairedBootstrap(active, good, .05, 10, 42, 2)
	two, secondErr := PairedBootstrap(active, good, .05, 10, 42, 2)
	if err != nil || secondErr != nil || !reflect.DeepEqual(one, two) || one.Status != "accept" || one.IntervalLowerPercent == nil || one.IntervalUpperPercent == nil {
		t.Fatalf("one=%#v two=%#v err=%v second=%v", one, two, err, secondErr)
	}
	bad := []float64{.70, .68, .72, .66, .71, .69, .73, .67, .70, .72}
	rejected, err := PairedBootstrap(active, bad, .05, 10, 42, 5)
	if err != nil || rejected.Status != "reject" {
		t.Fatalf("result=%#v err=%v", rejected, err)
	}
}

func TestPairedBootstrapFailsClosedOnInsufficientOrMismatchedSamples(t *testing.T) {
	result, err := PairedBootstrap([]float64{.8, .9}, []float64{.8}, .05, 2, 7, 5)
	if err != nil || result.Status != "insufficient" || result.IntervalLowerPercent != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
