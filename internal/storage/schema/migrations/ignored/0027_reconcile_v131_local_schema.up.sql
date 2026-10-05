-- Fork/release reconciliation: both branches previously used ignored version
-- 0026, for wisps.current_revision (fork) and dependency re-key (v1.3.1).
-- Keep the fork's frozen 0026 intact. A fresh 0027 is pending on BOTH cursor-26
-- lineages, so MigrateUp runs the merge-aware re-key before recording this step.
-- Repeat the guarded column addition to heal a release clone that consumed its
-- own 0026 without ever acquiring the fork's clone-local wisps column. On a fork
-- clone the column already exists and the guard makes this a no-op.
-- This step must stay unrecorded if either the re-key or column repair fails.
--
-- Ignored migration 0027: clone-local marker that forces one dependency-id
-- re-key pass with the duplicate-edge merge (gastownhall/beads#5268).
--
-- rekeyDependencyIDs (internal/storage/schema/dep_id_backfill.go) converges the
-- per-clone-random dependency ids that migration 0043's DEFAULT (UUID()) minted,
-- the data half of the #4259 fix. Until 1.3.0 it rewrote ids one row at a time
-- with no awareness of the ids already in the table, so a database holding the
-- same logical edge in two typed target columns -- legal, because depid keys on
-- (issue_id, resolved target) while uk_dep_issue_target / uk_dep_wisp_target /
-- uk_dep_external_target are per-column -- aborted mid-table with "duplicate
-- primary key given". The numbered migrations and their cursor rows commit
-- per-step, before the re-key tail runs, so the abort left the main cursor
-- durably at latest with only part of the table re-keyed: migrationWorkNeeded
-- then reported no work and the re-key never ran again.
--
-- While this version is still pending, migrationWorkNeeded returns true, so
-- every existing clone runs exactly one more MigrateUp pass and the fixed
-- (merge-aware) re-key heals it, including the clones that a pre-1.3.0 binary
-- left half-re-keyed at the latest main version. The pass is cheap on a
-- converged database: the re-key scan finds nothing to change and writes
-- nothing, which is why this marker needs none of the shipped-main-version
-- gating the aux-row-id passes carry (ignored 0009 / 0018) -- the dependency
-- re-key already ran on every migration pass before this change.
--
-- Recording the marker AFTER the re-key step is the other half of the fix:
-- ignoredSource.migrate runs later in MigrateUp than rekeyDependencyIDs, so a
-- re-key that fails for any reason leaves this version unrecorded and the pass
-- is retried on the next open instead of a database claiming it migrated.
--
-- The cursor table is dolt-ignored, so the marker is clone-local by
-- construction: every clone performs its own convergence pass, which is safe
-- precisely because the derivation and the duplicate-merge rule are both
-- deterministic functions of table content.
--
-- The marker itself changes nothing.
SELECT 1;

SET @needs_add = IF(
    (SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'wisps') > 0
    AND
    (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME = 'wisps'
          AND COLUMN_NAME = 'current_revision') = 0,
    1, 0
);
SET @sql = IF(@needs_add = 1,
    'ALTER TABLE wisps ADD COLUMN current_revision BIGINT NOT NULL DEFAULT 1',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
