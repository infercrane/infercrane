ALTER TABLE managed_spend_reservations
  DROP CONSTRAINT managed_spend_reservations_resource_type_check;

ALTER TABLE managed_spend_reservations
  ADD CONSTRAINT managed_spend_reservations_resource_type_check
  CHECK(resource_type IN ('deployment','optimization_campaign','sandbox'));

-- Customer activity and billing lifecycle are deliberately separate clocks.
-- Commands update last_active_at for the product UI, but they must not reset
-- the beginning of the currently billable running or standby interval.
ALTER TABLE native_sandboxes
  ADD COLUMN billing_state_since TIMESTAMPTZ;

UPDATE native_sandboxes
  SET billing_state_since=updated_at
  WHERE billing_state_since IS NULL;

ALTER TABLE native_sandboxes
  ALTER COLUMN billing_state_since SET NOT NULL;
