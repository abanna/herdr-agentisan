-- 0004: the routing decision a worker was launched with (ADR-001 A50,
-- NERD-5289): the role and provider it was decided for, the binding, model
-- id and effort chosen, the override that chose them, and the assignment and
-- policy it was made by. All NULL for a worker routing did not launch:
-- routing off, or a row bound before this migration. A bind of a retired or
-- lost id writes them afresh, so a reused id never inherits them.
ALTER TABLE workers ADD COLUMN routed_at INTEGER CHECK (routed_at IS NULL OR routed_at >= 0);
ALTER TABLE workers ADD COLUMN routed_role TEXT CHECK ((routed_role IS NULL) = (routed_at IS NULL));
ALTER TABLE workers ADD COLUMN routed_provider TEXT CHECK ((routed_provider IS NULL) = (routed_at IS NULL));
ALTER TABLE workers ADD COLUMN routed_binding TEXT CHECK ((routed_binding IS NULL) = (routed_at IS NULL));
ALTER TABLE workers ADD COLUMN routed_model_id TEXT CHECK ((routed_model_id IS NULL) = (routed_at IS NULL));
ALTER TABLE workers ADD COLUMN routed_effort TEXT CHECK ((routed_effort IS NULL) = (routed_at IS NULL));
-- The override, when one chose the binding or the effort: the binding it
-- named ('' when only the effort was set), whose it was (user or boss), and
-- the boss's effort ('' for none).
ALTER TABLE workers ADD COLUMN override_binding TEXT;
ALTER TABLE workers ADD COLUMN override_by TEXT;
ALTER TABLE workers ADD COLUMN override_effort TEXT;
-- The assignment's digest ('' for none) and the policy version.
ALTER TABLE workers ADD COLUMN assignment_digest TEXT CHECK ((assignment_digest IS NULL) = (routed_at IS NULL));
ALTER TABLE workers ADD COLUMN policy_version TEXT CHECK ((policy_version IS NULL) = (routed_at IS NULL));
