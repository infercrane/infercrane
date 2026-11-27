-- The results store (internal/resultstore): every measured cell as a
-- technique result record, so the planner's prior can rank setups by what
-- similar cells showed. A record holds numbers and short names only, never
-- prompts, answers, or request text.
--
-- A record is read inside its tenant. When its tenant set sharing to
-- 'numbers', the prior may also read it for other tenants, reduced in Go to
-- its numbers (resultstore.Record.NumbersOnly); it is never returned to them.
CREATE TABLE result_records(
  tenant_id TEXT NOT NULL,
  id TEXT NOT NULL CHECK(id ~ '^rr_[a-f0-9]{32}$'),
  cell_id TEXT NOT NULL CHECK(cell_id ~ '^cell_[a-f0-9]{24}$'),
  sharing TEXT NOT NULL DEFAULT 'private' CHECK(sharing IN ('private','numbers')),
  model_id TEXT NOT NULL CHECK(length(model_id) BETWEEN 1 AND 128),
  accelerator TEXT NOT NULL CHECK(length(accelerator) <= 128),
  engine TEXT NOT NULL CHECK(length(engine) <= 64),
  workload_kind TEXT NOT NULL CHECK(length(workload_kind) <= 64),
  load INTEGER NOT NULL CHECK(load >= 0),
  metric TEXT NOT NULL CHECK(length(metric) BETWEEN 1 AND 64),
  technique TEXT NOT NULL CHECK(length(technique) <= 64),
  claim TEXT NOT NULL CHECK(claim IN ('shown','not shown')),
  record JSONB NOT NULL CHECK(octet_length(record::text) <= 65536),
  recorded_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY(tenant_id, id)
);

CREATE INDEX result_records_by_cell ON result_records(tenant_id, metric, workload_kind, load);
CREATE INDEX result_records_newest ON result_records(tenant_id, recorded_at DESC, id);
CREATE INDEX result_records_shared ON result_records(metric, workload_kind, recorded_at DESC) WHERE sharing = 'numbers';

-- A record is immutable; only its sharing changes, in the column and in the
-- record's own "sharing" field together. A tenant may delete its records.
CREATE FUNCTION infercrane_result_records_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW.tenant_id, NEW.id, NEW.cell_id, NEW.model_id, NEW.accelerator, NEW.engine, NEW.workload_kind, NEW.load, NEW.metric,
         NEW.technique, NEW.claim, NEW.record - 'sharing', NEW.recorded_at, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.tenant_id, OLD.id, OLD.cell_id, OLD.model_id, OLD.accelerator, OLD.engine, OLD.workload_kind, OLD.load, OLD.metric,
         OLD.technique, OLD.claim, OLD.record - 'sharing', OLD.recorded_at, OLD.created_at)
     OR NEW.record->>'sharing' IS DISTINCT FROM NEW.sharing THEN
    RAISE EXCEPTION 'result records are immutable; only their sharing changes'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'result_records_immutable';
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER result_records_immutable BEFORE UPDATE ON result_records
  FOR EACH ROW EXECUTE FUNCTION infercrane_result_records_immutable();
