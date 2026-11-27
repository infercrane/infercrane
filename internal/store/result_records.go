package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/resultstore"
)

var _ resultstore.Store = (*Store)(nil)

// PutResultRecords adds sealed records to the tenant in one transaction. A
// record already there is not added again; its sharing is set to the one
// given, so importing again states the tenant's current choice. The claim
// column is the claim as decided when the row was written, kept for reading
// the table by hand; nothing reads it back. Every read decides the claim
// again with the technique contract's rule (Record.Claim), so a change to
// that rule reaches stored records.
func (s *Store) PutResultRecords(ctx context.Context, tenant string, records []resultstore.Record) (int, error) {
	tenant = resultTenant(tenant)
	if len(records) > resultstore.MaxPutRecords {
		return 0, fmt.Errorf("at most %d records at once", resultstore.MaxPutRecords)
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return 0, err
		}
		document, err := json.Marshal(record)
		if err != nil {
			return 0, err
		}
		var inserted bool
		err = tx.QueryRowContext(ctx, `INSERT INTO result_records(tenant_id,id,cell_id,sharing,model_id,accelerator,engine,workload_kind,load,metric,technique,claim,record,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?)
ON CONFLICT(tenant_id,id) DO UPDATE SET sharing=EXCLUDED.sharing, record=jsonb_set(result_records.record,'{sharing}',to_jsonb(EXCLUDED.sharing))
WHERE result_records.sharing IS DISTINCT FROM EXCLUDED.sharing
RETURNING (xmax = 0)`,
			tenant, record.ID, record.Cell.ID, record.Sharing, record.Model.ID, record.Accelerator(), record.Result.Software.Engine, record.Kind(), record.Load(),
			record.Metric(), record.Result.Technique, record.Claim(), string(document), record.RecordedAt).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			// Already there with the same sharing.
			continue
		}
		if err != nil {
			return 0, err
		}
		if inserted {
			added++
		}
	}
	return added, tx.Commit()
}

// ListResultRecords reads the tenant's own records, newest first.
func (s *Store) ListResultRecords(ctx context.Context, tenant string, filter resultstore.Filter) ([]resultstore.Record, error) {
	where, args := resultFilter(filter)
	query := `SELECT record,sharing FROM result_records WHERE tenant_id=?` + where + ` ORDER BY recorded_at DESC, id LIMIT ?`
	args = append([]any{resultTenant(tenant)}, append(args, filter.Bounded())...)
	return s.readResultRecords(ctx, query, args, filter, false)
}

// GetResultRecord reads one of the tenant's own records.
func (s *Store) GetResultRecord(ctx context.Context, tenant, id string) (resultstore.Record, error) {
	var document []byte
	var sharing string
	err := s.QueryRowContext(ctx, `SELECT record,sharing FROM result_records WHERE tenant_id=? AND id=?`, resultTenant(tenant), id).Scan(&document, &sharing)
	if errors.Is(err, sql.ErrNoRows) {
		return resultstore.Record{}, resultstore.ErrNotFound
	}
	if err != nil {
		return resultstore.Record{}, err
	}
	return decodeResultRecord(document, sharing)
}

// SharedResultRecords reads other tenants' records whose owners set sharing
// to numbers, each reduced by NumbersOnly before it leaves this function.
func (s *Store) SharedResultRecords(ctx context.Context, tenant string, filter resultstore.Filter) ([]resultstore.Record, error) {
	where, args := resultFilter(resultstore.Filter{Accelerator: filter.Accelerator, Engine: filter.Engine, Kind: filter.Kind, Metric: filter.Metric, Technique: filter.Technique,
		MinLoad: filter.MinLoad, MaxLoad: filter.MaxLoad})
	query := `SELECT record,sharing FROM result_records WHERE sharing='numbers' AND tenant_id<>?` + where + ` ORDER BY recorded_at DESC, id LIMIT ?`
	args = append([]any{resultTenant(tenant)}, append(args, filter.Bounded())...)
	return s.readResultRecords(ctx, query, args, filter, true)
}

func (s *Store) readResultRecords(ctx context.Context, query string, args []any, filter resultstore.Filter, shared bool) ([]resultstore.Record, error) {
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resultstore.Record
	for rows.Next() {
		var document []byte
		var sharing string
		if err = rows.Scan(&document, &sharing); err != nil {
			return nil, err
		}
		record, err := decodeResultRecord(document, sharing)
		if err != nil {
			return nil, err
		}
		if shared {
			record = record.NumbersOnly()
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func decodeResultRecord(document []byte, sharing string) (resultstore.Record, error) {
	var record resultstore.Record
	if err := json.Unmarshal(document, &record); err != nil {
		return resultstore.Record{}, err
	}
	record.Sharing = sharing
	return record, nil
}

// resultFilter is the SQL for the filter's indexed columns; the model and
// cell are matched on the record's columns too.
func resultFilter(filter resultstore.Filter) (string, []any) {
	var clauses []string
	var args []any
	add := func(column, value string) {
		if value != "" {
			clauses = append(clauses, "lower("+column+")=lower(?)")
			args = append(args, value)
		}
	}
	add("model_id", filter.Model)
	add("accelerator", filter.Accelerator)
	add("engine", filter.Engine)
	add("workload_kind", filter.Kind)
	add("metric", filter.Metric)
	add("technique", filter.Technique)
	add("cell_id", filter.Cell)
	if filter.MinLoad > 0 {
		clauses = append(clauses, "load>=?")
		args = append(args, filter.MinLoad)
	}
	if filter.MaxLoad > 0 {
		clauses = append(clauses, "load<=?")
		args = append(args, filter.MaxLoad)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

func resultTenant(tenant string) string {
	if tenant == "" {
		return "global"
	}
	return tenant
}
