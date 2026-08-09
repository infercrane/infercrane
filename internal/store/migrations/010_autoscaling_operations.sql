ALTER TABLE autoscaling_state
 ADD COLUMN desired_replicas INTEGER CHECK(desired_replicas >= 0),
 ADD COLUMN scale_generation BIGINT NOT NULL DEFAULT 0;
