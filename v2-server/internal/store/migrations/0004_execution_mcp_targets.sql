-- NULL denotes an execution without lifecycle MCP. A live target is keyed by the
-- session's original plugin IDs; changes replace a member, never its membership.
ALTER TABLE v2_execution_sessions
    ADD COLUMN mcp_target jsonb,
    ADD COLUMN mcp_generation bigint,
    ADD CHECK ((mcp_target IS NULL) = (mcp_generation IS NULL)),
    ADD CHECK (jsonb_typeof(mcp_target) = 'object' AND mcp_target <> '{}'::jsonb),
    ADD CHECK (mcp_generation BETWEEN 0 AND 9007199254740991);

CREATE INDEX v2_execution_live_mcp_members ON v2_execution_sessions
    USING gin (mcp_target) WHERE ended_at IS NULL AND mcp_target IS NOT NULL;
