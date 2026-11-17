package gputrace

import (
	"math"
	"sort"
)

type span struct{ start, end int64 }

// union merges spans sorted by start into disjoint spans. first[i] is the
// input index that opened merged span i.
func union(spans []span) (merged []span, first []int) {
	for index, current := range spans {
		if len(merged) > 0 && current.start <= merged[len(merged)-1].end {
			if current.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = current.end
			}
			continue
		}
		merged = append(merged, current)
		first = append(first, index)
	}
	return merged, first
}

func total(spans []span) int64 {
	var sum int64
	for _, current := range spans {
		sum += current.end - current.start
	}
	return sum
}

// intersection is the length two disjoint, sorted span lists share.
func intersection(left, right []span) int64 {
	var sum int64
	for i, j := 0, 0; i < len(left) && j < len(right); {
		start, end := max(left[i].start, right[j].start), min(left[i].end, right[j].end)
		if end > start {
			sum += end - start
		}
		if left[i].end < right[j].end {
			i++
		} else {
			j++
		}
	}
	return sum
}

// within is the length of disjoint, sorted spans inside [from, to).
func within(spans []span, from, to int64) int64 {
	index := sort.Search(len(spans), func(i int) bool { return spans[i].end > from })
	var sum int64
	for ; index < len(spans) && spans[index].start < to; index++ {
		start, end := max(spans[index].start, from), min(spans[index].end, to)
		if end > start {
			sum += end - start
		}
	}
	return sum
}

// percentile is the nearest-rank percentile of sorted values.
func percentile(sorted []int64, fraction float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	return sorted[max(0, min(rank, len(sorted)-1))]
}

func sortedCopy(values []int64) []int64 {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}

func round(value float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(value*scale) / scale
}

func ms(ns int64) float64 { return round(float64(ns)/1e6, 3) }

func us(ns int64) float64 { return round(float64(ns)/1e3, 2) }

func shareOf(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return round(math.Min(1, math.Max(0, float64(part)/float64(whole))), 4)
}

func pointer[T any](value T) *T { return &value }

// niceStep rounds a step up to 1, 2, or 5 times a power of ten.
func niceStep(value int64) int64 {
	if value <= 1 {
		return 1
	}
	magnitude := int64(1)
	for magnitude*10 <= value {
		magnitude *= 10
	}
	for _, factor := range []int64{1, 2, 5, 10} {
		if factor*magnitude >= value {
			return factor * magnitude
		}
	}
	return 10 * magnitude
}
