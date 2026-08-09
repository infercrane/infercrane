ALTER TABLE deployments
DROP CONSTRAINT deployments_min_replicas_check;

ALTER TABLE deployments
ADD CONSTRAINT deployments_min_replicas_check CHECK(min_replicas >= 0);

ALTER TABLE scaling_policies
DROP CONSTRAINT scaling_policies_min_replicas_check;

ALTER TABLE scaling_policies
ADD CONSTRAINT scaling_policies_min_replicas_check CHECK(min_replicas >= 0);
