-- 0008: the provider each worker was launched as (ADR-001 A66, NERD-5519):
-- claude or codex, the agent its launch ran, written at every bind, whether
-- or not routing decided the launch, so rotation and status need not infer
-- it. NULL for a row bound before this migration whose routing named none:
-- a row routing launched gets its decision's provider, which is the one it
-- ran, since routing never changes the provider (A50).
ALTER TABLE workers ADD COLUMN provider TEXT CHECK (provider IS NULL OR provider IN ('claude', 'codex'));
UPDATE workers SET provider = routed_provider WHERE routed_provider IN ('claude', 'codex');
