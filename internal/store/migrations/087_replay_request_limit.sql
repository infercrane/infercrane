-- A replay capture keeps the newest requests up to its limit. When the window
-- held more, the kept requests span less than the window, and a projection
-- over the whole window would understate the traffic rate.
--   request_limit_reached  true when the capture was cut off at its limit,
--                          false when it kept every request in the window,
--                          NULL for a trace captured before this was recorded.
ALTER TABLE replay_traces ADD COLUMN request_limit_reached BOOLEAN;
