package storage

const schemaSQL = `
CREATE TABLE IF NOT EXISTS batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sealed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS records (
    batch_id uuid NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
    record_id varchar(128) NOT NULL,
    sequence varchar(200000) NOT NULL,
    PRIMARY KEY (batch_id, record_id),
    CONSTRAINT records_ascii_id_chk CHECK (
        octet_length(record_id) BETWEEN 1 AND 128
        AND record_id !~ '[^A-Za-z0-9_-]'
    ),
    CONSTRAINT records_lowercase_sequence_chk CHECK (
        octet_length(sequence) BETWEEN 1 AND 200000
        AND sequence ~ '^[a-z]+$'
    )
);

CREATE TABLE IF NOT EXISTS reports (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id uuid NOT NULL UNIQUE REFERENCES batches(id),
    algorithm text NOT NULL,
    input_hash char(64) NOT NULL,
    frozen_input jsonb NOT NULL,
    labels jsonb NOT NULL,
    sealed_at timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION enforce_batch_total_length()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    total integer;
BEGIN
    SELECT COALESCE(SUM(octet_length(sequence)), 0) INTO total
    FROM records
    WHERE batch_id = COALESCE(NEW.batch_id, OLD.batch_id);

    IF total > 500000 THEN
        RAISE EXCEPTION 'batch total sequence length exceeds 500000'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS records_total_length_aiu ON records;
CREATE CONSTRAINT TRIGGER records_total_length_aiu
    AFTER INSERT OR UPDATE ON records
    DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW
    EXECUTE FUNCTION enforce_batch_total_length();

CREATE OR REPLACE FUNCTION enforce_batch_total_length_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    total integer;
BEGIN
    SELECT COALESCE(SUM(octet_length(sequence)), 0) INTO total
    FROM records WHERE batch_id = OLD.batch_id;

    IF total > 500000 THEN
        RAISE EXCEPTION 'batch total sequence length exceeds 500000'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS records_total_length_ad ON records;
CREATE CONSTRAINT TRIGGER records_total_length_ad
    AFTER DELETE ON records
    DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW
    EXECUTE FUNCTION enforce_batch_total_length_delete();

CREATE OR REPLACE FUNCTION touch_batch()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    UPDATE batches
       SET updated_at = now()
     WHERE id = COALESCE(NEW.batch_id, OLD.batch_id);
    RETURN COALESCE(NEW, OLD);
END;
$$;

DROP TRIGGER IF EXISTS records_touch_batch ON records;
CREATE CONSTRAINT TRIGGER records_touch_batch
    AFTER INSERT OR UPDATE OR DELETE ON records
    DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW
    EXECUTE FUNCTION touch_batch();

CREATE OR REPLACE FUNCTION block_report_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'reports are immutable'
        USING ERRCODE = 'insufficient_privilege';
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS reports_block_row_mutation ON reports;
CREATE TRIGGER reports_block_row_mutation
    BEFORE UPDATE OR DELETE ON reports
    FOR EACH ROW
    EXECUTE FUNCTION block_report_mutation();

DROP TRIGGER IF EXISTS reports_block_truncate ON reports;
CREATE TRIGGER reports_block_truncate
    BEFORE TRUNCATE ON reports
    FOR EACH STATEMENT
    EXECUTE FUNCTION block_report_mutation();
`
