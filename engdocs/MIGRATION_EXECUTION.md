# Frozen migration execution and replay

Historical migration files and their recorded content hashes remain immutable.
Runtime execution can require an exact-identity compatibility adapter: migration
0059 has a bounded graph implementation, and migration 0073 adds only missing
nullable LONGTEXT receipt columns. Both adapters validate the source, filename,
version and original content hash. They do not grant migration consent, bypass
existing locks or create another schema version.

For 0073, both existing columns are inspected before DDL. Correct columns and
receipt values are retained; incompatible types, nullability, defaults or
generated expressions refuse. Interrupted partial shapes converge by adding
only the missing column. The normal migration runner records the original
frozen SQL hash and commits through its existing path.

This does not make the frozen 0073 bytes idempotent. A direct replay still
refuses duplicate columns, which an explicit real-engine test characterizes.
The SQL-file replay fixture therefore keeps raw 0059 as its independent graph
oracle and routes only exact main 0073 through the existing locked production
executor after a private committed cursor rewind. It requires one actual
migration and restoration of the original hash; no file is silently skipped.
The separate raw 0067 two-pass test retains seeded revision/version assertions;
the whole-chain fixture repeats every original graph and data assertion.

All such cursor rewinds and database experiments belong to isolated test
fixtures. They are not production repair commands. Existing databases at 73/28
need no migration for the packaging or downstream binary update.
