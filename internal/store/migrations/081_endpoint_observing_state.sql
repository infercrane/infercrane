-- An endpoint whose active serving plan contains only observe-only bindings
-- never publishes a route by design. The reconciler records it as
-- 'observing' instead of 'degraded', so widen the observed-state CHECK.
ALTER TABLE endpoints
  DROP CONSTRAINT endpoints_observed_state_check,
  ADD CONSTRAINT endpoints_observed_state_check
    CHECK(observed_state IN ('pending','serving','observing','degraded','suspended','deleted'));
