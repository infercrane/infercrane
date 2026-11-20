package benchmark

import (
	"math"
	"testing"
)

func TestStudentTQuantileMatchesPublishedValues(t *testing.T) {
	cases := []struct {
		p    float64
		df   int
		want float64
	}{
		{0.975, 1, 12.7062},
		{0.975, 2, 4.3027},
		{0.975, 3, 3.1824},
		{0.975, 4, 2.7764},
		{0.975, 9, 2.2622},
		{0.95, 10, 1.8125},
		{0.995, 3, 5.8409},
		{0.995, 20, 2.8453},
		{0.9995, 5, 6.8688},
	}
	for _, test := range cases {
		if got := StudentTQuantile(test.p, test.df); math.Abs(got-test.want) > 5e-4 {
			t.Errorf("StudentTQuantile(%v, %d) = %.5f, want %.4f", test.p, test.df, got, test.want)
		}
	}
}

func TestStudentTQuantileAgreesWithTheIntervalTable(t *testing.T) {
	table := []float64{0, 12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262}
	for df := 1; df < len(table); df++ {
		if got := StudentTQuantile(0.975, df); math.Abs(got-table[df]) > 1e-3 {
			t.Errorf("df %d: %.4f, table %.3f", df, got, table[df])
		}
	}
}

func TestStudentTQuantileRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		p  float64
		df int
	}{{0.4, 3}, {1, 3}, {0.975, 0}, {math.NaN(), 3}} {
		if got := StudentTQuantile(test.p, test.df); !math.IsNaN(got) {
			t.Errorf("StudentTQuantile(%v, %d) = %v, want NaN", test.p, test.df, got)
		}
	}
	if got := StudentTQuantile(0.5, 4); got != 0 {
		t.Errorf("median = %v, want 0", got)
	}
}
