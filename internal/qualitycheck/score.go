package qualitycheck

import (
	"regexp"
	"strings"
	"unicode"
)

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?(</think>|$)`)

// Agreement scores how closely a candidate answer matches the reference
// answer, from 0 to 1. It is the token-level F1 of the two answers after
// lowercasing and splitting on anything that is not a letter or digit, so
// case, punctuation, and spacing do not count as a difference. Reasoning
// blocks are removed first. Two empty answers agree; one empty answer does
// not.
func Agreement(reference, candidate string) float64 {
	left, right := tokens(reference), tokens(candidate)
	if len(left) == 0 && len(right) == 0 {
		return 1
	}
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	counts := make(map[string]int, len(left))
	for _, token := range left {
		counts[token]++
	}
	common := 0
	for _, token := range right {
		if counts[token] > 0 {
			counts[token]--
			common++
		}
	}
	if common == 0 {
		return 0
	}
	precision := float64(common) / float64(len(right))
	recall := float64(common) / float64(len(left))
	return 2 * precision * recall / (precision + recall)
}

func tokens(answer string) []string {
	answer = thinkBlock.ReplaceAllString(answer, " ")
	return strings.FieldsFunc(strings.ToLower(answer), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
