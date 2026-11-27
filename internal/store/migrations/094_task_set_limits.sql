-- A task set (an eval set graded answer_line) holds up to 1,000 items and
-- 1 MiB (internal/qualitycheck, MaxTaskPrompts and MaxEvalSetBytes). The
-- table kept the general eval set's bounds, 200 prompts and 512 KiB, so a
-- full task set could not be stored. The limits per kind stay in the
-- decoder; the table holds the largest kind.
ALTER TABLE quality_eval_sets DROP CONSTRAINT IF EXISTS quality_eval_sets_prompt_count_check;
ALTER TABLE quality_eval_sets ADD CONSTRAINT quality_eval_sets_prompt_count_check CHECK (prompt_count BETWEEN 1 AND 1000);
ALTER TABLE quality_eval_sets DROP CONSTRAINT IF EXISTS quality_eval_sets_body_check;
ALTER TABLE quality_eval_sets ADD CONSTRAINT quality_eval_sets_body_check CHECK (octet_length(body) BETWEEN 1 AND 1048576);
