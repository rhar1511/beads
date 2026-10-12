-- An applied metadata transition and its recovery receipt commit together.
-- Receipt IDs are unique across a workspace; the issue row remains the source
-- of truth for the marker and its row_lock revision.
CREATE TABLE IF NOT EXISTS issue_transition_receipts (
    receipt_id VARCHAR(255) NOT NULL PRIMARY KEY,
    issue_id VARCHAR(255) NOT NULL,
    scope VARCHAR(255) NOT NULL,
    kind VARCHAR(255) NOT NULL,
    actor VARCHAR(255) NOT NULL,
    expected_version BIGINT NOT NULL,
    to_version BIGINT NOT NULL,
    metadata_key VARCHAR(255) NOT NULL,
    expected_json LONGTEXT NULL,
    value_json LONGTEXT NULL,
    payload_json LONGTEXT NOT NULL,
    INDEX idx_issue_transition_receipts_issue (issue_id)
);
