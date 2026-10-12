-- Canonical request and receipt bodies for exact multi-field source patches.
-- NULL in both columns identifies the compatible legacy one-key receipt row.
ALTER TABLE issue_transition_receipts
    ADD COLUMN request_json LONGTEXT NULL,
    ADD COLUMN receipt_json LONGTEXT NULL;
