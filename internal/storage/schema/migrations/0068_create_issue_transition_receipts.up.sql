-- Durable operation receipts for issue revision-fenced metadata transitions.
--
-- A receipt is inserted with its final post-write row_lock token in the same
-- SQL transaction as the issue update. The receipt ID is the primary key so a
-- retry can reconcile an unknown response without reapplying the marker.
--
-- This is a normal Dolt-versioned table. It intentionally has no foreign key
-- to issues: deleting an issue must not cascade away the receipt that proves a
-- transition already committed. The application exposes no update/delete
-- operation for committed receipts; the table is not clone-local or retained
-- in the events journal.
CREATE TABLE IF NOT EXISTS issue_transition_receipts (
    receipt_id VARBINARY(191) NOT NULL PRIMARY KEY,
    issue_id VARCHAR(255) NOT NULL,
    scope VARCHAR(255) NOT NULL,
    kind VARCHAR(64) NOT NULL,
    actor VARCHAR(255) NOT NULL,
    metadata_key VARCHAR(255) NOT NULL,
    from_revision BIGINT NOT NULL,
    to_revision BIGINT NOT NULL,
    expected_present TINYINT(1) NOT NULL,
    expected_json LONGTEXT NULL,
    value_present TINYINT(1) NOT NULL,
    value_json LONGTEXT NULL,
    payload_json LONGTEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_issue_transition_receipts_issue_created (issue_id, created_at)
);
