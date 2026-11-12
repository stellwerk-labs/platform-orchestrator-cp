-- +goose Up
CREATE TABLE installation_maintenance (
 singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),
 revision BIGINT NOT NULL DEFAULT 0 CHECK(revision >= 0),
 enabled BOOLEAN NOT NULL DEFAULT true
);
INSERT INTO installation_maintenance(singleton) VALUES(true);
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM installation_maintenance WHERE revision>0 AND enabled) THEN
  RAISE EXCEPTION 'disable installation maintenance before removing the plugin barrier';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE installation_maintenance;
