package resultstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LocalTenant is the one tenant of a Local store.
const LocalTenant = "local"

// maxLocalLine bounds one line of a local store file.
const maxLocalLine = 1 << 20

// Local is a store in one file of JSON Lines, one sealed record per line, for
// the CLI without a control plane and for the planner gym. It holds the
// records of one person or one harness: its only tenant is LocalTenant, and
// it shares nothing.
type Local struct{ Path string }

var _ Store = Local{}

// Load reads every record in the file. A missing file is an empty store.
func (l Local) Load() ([]Record, error) {
	file, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var records []Record
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxLocalLine)
	for line := 1; scanner.Scan(); line++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		record, err := decodeRecord(text)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", l.Path, line, err)
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

// PutResultRecords adds records whose IDs are not in the file yet and writes
// the file again in one rename.
func (l Local) PutResultRecords(_ context.Context, tenant string, records []Record) (int, error) {
	if tenant != LocalTenant {
		return 0, fmt.Errorf("a local store holds tenant %q only", LocalTenant)
	}
	if len(records) > MaxPutRecords {
		return 0, fmt.Errorf("at most %d records at once", MaxPutRecords)
	}
	existing, err := l.Load()
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, record := range existing {
		seen[record.ID] = true
	}
	added := 0
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return 0, err
		}
		if seen[record.ID] {
			continue
		}
		seen[record.ID] = true
		existing = append(existing, record)
		added++
	}
	if added == 0 {
		return 0, nil
	}
	slices.SortFunc(existing, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	var body bytes.Buffer
	for _, record := range existing {
		line, err := json.Marshal(record)
		if err != nil {
			return 0, err
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return 0, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(l.Path), ".results-*.jsonl")
	if err != nil {
		return 0, err
	}
	defer os.Remove(temporary.Name())
	if _, err = temporary.Write(body.Bytes()); err == nil {
		err = temporary.Close()
	}
	if err != nil {
		return 0, err
	}
	return added, os.Rename(temporary.Name(), l.Path)
}

// ListResultRecords reads the file's records that pass the filter.
func (l Local) ListResultRecords(_ context.Context, tenant string, filter Filter) ([]Record, error) {
	if tenant != LocalTenant {
		return nil, nil
	}
	records, err := l.Load()
	if err != nil {
		return nil, err
	}
	return Newest(records, filter), nil
}

// GetResultRecord reads one record.
func (l Local) GetResultRecord(_ context.Context, tenant, id string) (Record, error) {
	if tenant != LocalTenant {
		return Record{}, ErrNotFound
	}
	records, err := l.Load()
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.ID == id {
			return record, nil
		}
	}
	return Record{}, ErrNotFound
}

// SharedResultRecords is empty: a local store has no other tenants.
func (Local) SharedResultRecords(context.Context, string, Filter) ([]Record, error) {
	return nil, nil
}

func decodeRecord(body []byte) (Record, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, err
	}
	return record, record.Validate()
}
