-- +goose Up
-- Fence the persisted observation response format. Older Core binaries reject
-- the deletion reason and cannot safely recover a pruned configuration receipt.
SELECT 1;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM environment_observation_closures WHERE response->>'reason'='deleted_target')
 OR EXISTS(SELECT 1 FROM environment_observation_grant_commands WHERE jsonb_array_length(response->'pruned_environment_ids')>0) THEN
  RAISE EXCEPTION 'deletion-aware observation responses require a coordinated restore before downgrading Core';
 END IF;
END $$;
-- +goose StatementEnd
