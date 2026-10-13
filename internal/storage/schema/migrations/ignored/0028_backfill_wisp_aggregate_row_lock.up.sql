-- Ignored migration 0028: give clone-local wisps a usable aggregate revision.
-- This is the ignored-plane twin of synced migration 0068.
-- Revision bookkeeping must preserve the authored edit timestamp.
UPDATE wisps SET row_lock = 1, updated_at = updated_at WHERE row_lock = 0;
