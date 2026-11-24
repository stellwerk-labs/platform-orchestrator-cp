-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION retain_native_runner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.type = 'native-agent' THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'runners_native_retention',
            MESSAGE = 'native Runner registration retains execution authority';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER retain_native_runner BEFORE DELETE ON runners
    FOR EACH ROW EXECUTE FUNCTION retain_native_runner();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM runners WHERE type = 'native-agent') THEN
        RAISE EXCEPTION 'cannot discard native Runner registration retention';
    END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER retain_native_runner ON runners;
DROP FUNCTION retain_native_runner();
