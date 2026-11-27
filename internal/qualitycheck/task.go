package qualitycheck

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A task set scores whether answers are right, not whether they match the
// reference: every item has one gold answer, a number or a letter from A to
// D, and each side's answers are scored against it. Two setups are then
// compared item by item. The scoring and the verdict are the rule of the map
// on one H100 (tools/fast-track-map/quality.py), which a task set of 1,000
// items settled to within about one point.

const (
	// GradingAnswerLine scores an answer by its last "Answer:" line: the
	// first number after it for a number item, the first letter A to D that
	// stands alone after it for a multiple-choice item. A text without that
	// line is scored on its last number for a number item and is wrong for a
	// multiple-choice item.
	GradingAnswerLine = "answer_line"
	// TaskMargin is how close to zero, in points of the share of items right,
	// the whole 95% interval of the paired difference must lie for "no
	// measurable difference".
	TaskMargin = 0.03
	// HeldMargin is the most a setup that changes answers (other weights or
	// precision) may lose and still count as holding them: the lower end of
	// the paired difference's 95% interval must lie above it.
	HeldMargin = 0.01
	// MaxTaskPrompts bounds the items of a task set; other sets keep
	// MaxPrompts.
	MaxTaskPrompts = 1000
	// maxTaskTokens is the answer room a task set may give: answers with
	// working need about 1,000 tokens, and a third were cut at 384 or 512.
	maxTaskTokens = 2048
)

// Task verdicts, by where the paired difference's 95% interval lies.
const (
	TaskBetter       = "better"
	TaskWorse        = "worse"
	TaskNoDifference = "no_measurable_difference"
	TaskNotSettled   = "not_settled"
	// TaskHeld and TaskNotShownHeld are the verdicts of the rule for setups
	// that change answers (RequireHeld): held within HeldMargin, or not shown
	// to be, which never passes.
	TaskHeld         = "held"
	TaskNotShownHeld = "not_shown_held"
)

// Task rules: how a task comparison's interval is read.
const (
	// RuleWithinMargin: no measurable difference when the whole interval
	// lies inside ±TaskMargin.
	RuleWithinMargin = "within_3_points"
	// RuleHeld: for a setup that changes answers, held only when the
	// interval's lower end lies above −HeldMargin.
	RuleHeld = "held_within_1_point"
)

var (
	answerMarker = regexp.MustCompile(`(?i)answer\s*[:：]`)
	answerNumber = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?|-?\.\d+`)
)

// answerLineScore is 1 when the answer gives the gold answer, else 0.
func answerLineScore(gold, answer string) float64 {
	marks := answerMarker.FindAllStringIndex(answer, -1)
	after := ""
	if len(marks) > 0 {
		after = answer[marks[len(marks)-1][1]:]
	}
	if letterGold(gold) {
		if len(marks) > 0 && firstLetter(after) == strings.TrimSpace(gold) {
			return 1
		}
		return 0
	}
	var given string
	if len(marks) > 0 {
		if found := answerNumber.FindString(after); found != "" {
			given = found
		}
	} else if found := answerNumber.FindAllString(answer, -1); len(found) > 0 {
		given = found[len(found)-1]
	}
	value, valueOK := taskNumber(given)
	want, wantOK := taskNumber(gold)
	if valueOK && wantOK && math.Abs(value-want) < 1e-6 {
		return 1
	}
	return 0
}

func letterGold(gold string) bool {
	gold = strings.TrimSpace(gold)
	return len(gold) == 1 && gold[0] >= 'A' && gold[0] <= 'D'
}

// firstLetter is the first of A to D in text with no letter on either side.
func firstLetter(text string) string {
	letter := func(value rune) bool { return value < unicode.MaxASCII && unicode.IsLetter(value) }
	previous := rune(0)
	for index, current := range text {
		if current >= 'A' && current <= 'D' && !letter(previous) {
			next, _ := utf8.DecodeRuneInString(text[index+utf8.RuneLen(current):])
			if !letter(next) {
				return string(current)
			}
		}
		previous = current
	}
	return ""
}

// taskNumber reads a number as the task set writes it: thousands commas
// dropped, a trailing full stop ignored.
func taskNumber(text string) (float64, bool) {
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimRight(strings.ReplaceAll(text, ",", ""), "."), 64)
	return value, err == nil
}

// validTaskGold accepts a number or a single letter from A to D.
func validTaskGold(gold string) bool {
	if letterGold(gold) {
		return true
	}
	_, ok := taskNumber(strings.TrimSpace(gold))
	return ok
}

// TaskComparison compares two setups on a task set item by item. Scores are
// the share of items right; Difference is the candidate's minus the
// reference's, and Low and High bound its 95% interval.
type TaskComparison struct {
	ReferenceScore     float64 `json:"reference_score"`
	CandidateScore     float64 `json:"candidate_score"`
	Difference         float64 `json:"difference"`
	Low                float64 `json:"low"`
	High               float64 `json:"high"`
	Margin             float64 `json:"margin"`
	Verdict            string  `json:"verdict"`
	Items              int     `json:"items"`
	OnlyReferenceRight int     `json:"only_reference_right"`
	OnlyCandidateRight int     `json:"only_candidate_right"`
	// Rule is how the interval was read (RuleWithinMargin or RuleHeld).
	Rule string `json:"rule"`
	// ServersPerSide is how many servers of each setup answered the items:
	// the server pairs pooled into the verdict (PooledTask). A kept live
	// control is one server that answered once per pair.
	ServersPerSide int `json:"servers_per_side"`
	// PoolRefused says why pairs answered earlier were not pooled; the
	// verdict then rests on the servers asked last.
	PoolRefused string `json:"pool_refused,omitempty"`
}

// RequireHeld reads a comparison by the rule for setups that change answers:
// worse when the whole interval is below zero, better when it is above zero,
// held when its lower end lies above −HeldMargin, and otherwise not shown to
// hold, never "no measurable difference". A setup that gives other answers
// must show they hold; an interval too wide to show it does not pass.
func RequireHeld(task TaskComparison) TaskComparison {
	task.Rule, task.Margin = RuleHeld, HeldMargin
	switch {
	case task.Items < 2:
		task.Verdict = TaskNotShownHeld
	case task.High < 0:
		task.Verdict = TaskWorse
	case task.Low > 0:
		task.Verdict = TaskBetter
	case task.Low > -HeldMargin:
		task.Verdict = TaskHeld
	default:
		task.Verdict = TaskNotShownHeld
	}
	return task
}

// PairedTask is the map's rule: the mean of the per-item differences
// (candidate minus reference) with a 95% interval of 1.96 standard errors.
// Better or worse when the interval excludes zero; no measurable difference
// when it lies inside the margin; otherwise not settled. Fewer than two items
// settle nothing.
func PairedTask(reference, candidate []float64) TaskComparison {
	count := min(len(reference), len(candidate))
	result := TaskComparison{Items: count, Margin: TaskMargin, Verdict: TaskNotSettled, Rule: RuleWithinMargin, ServersPerSide: 1}
	if count == 0 {
		return result
	}
	differences := make([]float64, count)
	for index := range count {
		result.ReferenceScore += reference[index]
		result.CandidateScore += candidate[index]
		differences[index] = candidate[index] - reference[index]
		switch {
		case reference[index] > candidate[index]:
			result.OnlyReferenceRight++
		case candidate[index] > reference[index]:
			result.OnlyCandidateRight++
		}
		result.Difference += differences[index]
	}
	total := float64(count)
	result.ReferenceScore, result.CandidateScore, result.Difference = round(result.ReferenceScore/total), round(result.CandidateScore/total), result.Difference/total
	if count < 2 {
		result.Low, result.High = math.Inf(-1), math.Inf(1)
		result.Difference = round(result.Difference)
		return result
	}
	squared := 0.0
	for _, value := range differences {
		squared += (value - result.Difference) * (value - result.Difference)
	}
	errorOfMean := math.Sqrt(squared / (total - 1) / total)
	low, high := result.Difference-1.96*errorOfMean, result.Difference+1.96*errorOfMean
	switch {
	case low > 0:
		result.Verdict = TaskBetter
	case high < 0:
		result.Verdict = TaskWorse
	case -TaskMargin <= low && high <= TaskMargin:
		result.Verdict = TaskNoDifference
	}
	result.Difference, result.Low, result.High = round(result.Difference), round(low), round(high)
	return result
}
