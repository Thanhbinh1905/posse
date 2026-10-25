-- +goose Up
-- A Project snapshot records open and recent Notices and Decisions. Every
-- change to a recorded column replaces the Project's history revision with a
-- fresh random value, so an unchanged snapshot is recognized without reading
-- or hashing that history. Random values never repeat a recorded revision
-- after a rebuild or a restored backup, as a counter could. Trigger bodies
-- avoid conflict clauses, which an outer statement's conflict policy overrides.
CREATE TABLE project_history_revisions (
    project_id INTEGER PRIMARY KEY,
    revision INTEGER NOT NULL
);
INSERT INTO project_history_revisions(project_id, revision) SELECT id, random() FROM projects;
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_project_insert AFTER INSERT ON projects
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_notice_insert AFTER INSERT ON notices
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_notice_update 
AFTER UPDATE OF id, project_id, task_id, kind, summary, data_json, created_at, delivered_at, acked_at ON notices
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_notice_delete AFTER DELETE ON notices
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_decision_insert AFTER INSERT ON decisions
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_decision_update AFTER UPDATE ON decisions
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
    UPDATE project_history_revisions SET revision = random() WHERE project_id = NEW.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT NEW.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = NEW.project_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_history_revision_decision_delete AFTER DELETE ON decisions
BEGIN
    UPDATE project_history_revisions SET revision = random() WHERE project_id = OLD.project_id;
    INSERT INTO project_history_revisions(project_id, revision) SELECT OLD.project_id, random()
        WHERE NOT EXISTS (SELECT 1 FROM project_history_revisions WHERE project_id = OLD.project_id);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER project_history_revision_decision_delete;
DROP TRIGGER project_history_revision_decision_update;
DROP TRIGGER project_history_revision_decision_insert;
DROP TRIGGER project_history_revision_notice_delete;
DROP TRIGGER project_history_revision_notice_update;
DROP TRIGGER project_history_revision_notice_insert;
DROP TRIGGER project_history_revision_project_insert;
DROP TABLE project_history_revisions;
