package benchmark

import "math"

// StudentTQuantile returns t such that P(T ≤ t) = p for Student's t
// distribution with df degrees of freedom. It serves two-sided intervals, so
// p must be in [0.5, 1) and df at least 1; anything else returns NaN.
// Campaign comparisons widen their intervals for several candidates, which
// needs quantiles beyond the fixed 95% table in confidence95.
func StudentTQuantile(p float64, df int) float64 {
	if df < 1 || math.IsNaN(p) || p < 0.5 || p >= 1 {
		return math.NaN()
	}
	if p == 0.5 {
		return 0
	}
	// The CDF is increasing in t; bisect on [0, high] after growing high
	// until it brackets p.
	low, high := 0.0, 1.0
	for studentTCDF(high, df) < p {
		high *= 2
		if high > 1e12 {
			return math.Inf(1)
		}
	}
	for range 200 {
		middle := (low + high) / 2
		if studentTCDF(middle, df) < p {
			low = middle
		} else {
			high = middle
		}
		if high-low <= 1e-12*math.Max(1, high) {
			break
		}
	}
	return (low + high) / 2
}

// studentTCDF is P(T ≤ t) for t ≥ 0.
func studentTCDF(t float64, df int) float64 {
	v := float64(df)
	x := v / (v + t*t)
	return 1 - 0.5*regularizedIncompleteBeta(x, v/2, 0.5)
}

// regularizedIncompleteBeta is I_x(a, b), evaluated with the continued
// fraction that converges quickly on the side of x it is used on.
func regularizedIncompleteBeta(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	lbeta, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	front := math.Exp(lbeta - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(x, a, b) / a
	}
	return 1 - front*betaContinuedFraction(1-x, b, a)/b
}

func betaContinuedFraction(x, a, b float64) float64 {
	const tiny = 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 300; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1) < 1e-15 {
			break
		}
	}
	return h
}
