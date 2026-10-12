-- Migration 0068: make every durable aggregate revision immediately usable.
-- row_lock is equality-only, so legacy zero rows may share the same non-zero
-- backfill token; the next supported mutation remints a random token.
-- Revision bookkeeping must preserve the authored edit timestamp.
UPDATE issues SET row_lock = 1, updated_at = updated_at WHERE row_lock = 0;
