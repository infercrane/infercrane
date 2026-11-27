package specswitch

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// minimumShare is the smallest share the switch hands an engine: below it
// the sleeping engines keep so much that the design does not fit the GPU.
const minimumShare = 0.5

// ParseProbe reads "used, total" in MiB, as nvidia-smi's CSV prints it for
// one GPU.
func ParseProbe(output string) (used, total float64, err error) {
	line := strings.TrimSpace(output)
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		return 0, 0, errors.New("the memory probe printed more than one GPU")
	}
	parts := strings.Split(line, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("the memory probe printed %q, not \"used, total\"", line)
	}
	used, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err == nil {
		total, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	}
	if err != nil || used < 0 || total <= 0 || used > total {
		return 0, 0, fmt.Errorf("the memory probe printed %q", line)
	}
	return used, total, nil
}

// MemoryShare is the share of GPU memory an engine may take so that it fits
// awake beside the others asleep. usedMiB is the GPU's used memory while
// `asleep` engines sleep and none is awake; each is taken to keep an equal
// part of it, and `others` engines will sleep beside the awake one. The share
// is rounded down to a thousandth and capped at maxShare.
func MemoryShare(usedMiB, totalMiB float64, asleep, others int, marginMiB, maxShare float64) (float64, error) {
	if asleep < 1 || others < 0 || totalMiB <= 0 {
		return 0, errors.New("a share needs at least one sleeping engine and the GPU's total memory")
	}
	keptEach := usedMiB / float64(asleep)
	share := math.Floor((totalMiB-float64(others)*keptEach-marginMiB)/totalMiB*1000) / 1000
	share = math.Min(share, maxShare)
	if share < minimumShare {
		return 0, fmt.Errorf("the sleeping engines keep %.0f MiB each of %.0f MiB: an awake engine would get %.3f of the GPU", keptEach, totalMiB, share)
	}
	return share, nil
}

// WithShare returns the command with the memory share in place of
// MemoryShareToken.
func WithShare(command []string, share float64) []string {
	out := make([]string, len(command))
	value := strconv.FormatFloat(share, 'f', 3, 64)
	for index, arg := range command {
		out[index] = strings.ReplaceAll(arg, MemoryShareToken, value)
	}
	return out
}
