# ADR 0001: Separate control and request data planes

Status: accepted

InferCrane owns deployment intent and reconciliation. An upstream router owns worker
selection. The stable gateway performs only authentication, alias resolution, model rewriting,
stream-safe forwarding, and accounting. This keeps provider and routing implementations
replaceable and lets existing serving continue during a control-loop failure.

