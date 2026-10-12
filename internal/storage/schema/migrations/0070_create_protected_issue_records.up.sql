-- Explicit source protection is storage-managed state. It is kept outside
-- issue content so imports, exports, labels, metadata, and arbitrary payloads
-- cannot create or clear it. Deliberately no foreign key: deletion must read
-- the classification before it mutates or cascades any issue rows.
CREATE TABLE IF NOT EXISTS protected_issue_records (
    issue_id VARCHAR(255) NOT NULL PRIMARY KEY,
    protection_class VARCHAR(64) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);
