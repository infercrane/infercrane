package store

import "testing"

func TestReducedCanaryWeightsInterleave(t *testing.T) {
	for _, test := range []struct{ left, right, wantLeft, wantRight int }{{500, 9500, 1, 19}, {250, 9750, 1, 39}, {1, 9999, 1, 9999}, {300, 9700, 3, 97}} {
		left, right := reducedWeights(test.left, test.right)
		if left != test.wantLeft || right != test.wantRight {
			t.Fatalf("reducedWeights(%d, %d) = %d, %d", test.left, test.right, left, right)
		}
	}
}
