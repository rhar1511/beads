-- The complete canonical request and receipt also live in payload_json, so
-- dropping the indexed convenience columns preserves exact replay evidence.
-- Patch rows retain their reserved metadata_key marker and remain conflicts to
-- the legacy one-key API while this extension is rolled back.
ALTER TABLE issue_transition_receipts
    DROP COLUMN receipt_json,
    DROP COLUMN request_json;
