-- +goose Up
-- The agent kind and session of the Project's Lead, recorded at launch and
-- refreshed from Herdr observations, so an explicit restore brings back the
-- Lead that was running instead of the configured lead.kind.
ALTER TABLE project_runtime ADD COLUMN lead_agent_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE project_runtime ADD COLUMN lead_agent_session TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_runtime DROP COLUMN lead_agent_session;
ALTER TABLE project_runtime DROP COLUMN lead_agent_kind;
