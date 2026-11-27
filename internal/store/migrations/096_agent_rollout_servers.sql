-- A guarded rollout on an agent's endpoint starts the proven setup as a
-- server the agent owns and keeps (start_server), and deletes it again on a
-- rollback (stop_server). Both are bounded specs like every agent command.
ALTER TABLE agent_commands
  DROP CONSTRAINT agent_commands_kind_check,
  ADD CONSTRAINT agent_commands_kind_check CHECK(kind IN ('run_candidate','measure_window','start_server','stop_server'));
