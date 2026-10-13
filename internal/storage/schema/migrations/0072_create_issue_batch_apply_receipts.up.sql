-- Durable, immutable results for caller-identified issue batch applications.
-- The receipt is inserted in the same transaction as the batch, including
-- batches whose domain items only touch the ignored wisp plane or are no-ops.
-- It has no foreign key so later issue deletion cannot erase retry history.
CREATE TABLE IF NOT EXISTS issue_batch_apply_receipts (
    receipt_id VARBINARY(191) NOT NULL PRIMARY KEY,
    request_digest CHAR(64) NOT NULL,
    result_version SMALLINT NOT NULL,
    result_blob LONGBLOB NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_issue_batch_apply_receipts_created (created_at)
);
