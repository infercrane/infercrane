package router

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// WorkerSetHash identifies the exact strategy and worker URLs published by a
// router generation. Lifecycle code uses it as durable drain evidence.
func WorkerSetHash(strategy string, workers []string) string {
	workers = append([]string(nil), workers...)
	sort.Strings(workers)
	sum := sha256.Sum256([]byte(strategy + "\x00" + strings.Join(workers, "\x00")))
	return hex.EncodeToString(sum[:])
}
