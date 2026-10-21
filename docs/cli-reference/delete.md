---
title: "bd delete"
description: "Delete one or more issues and clean up references"
---

{/* AUTO-GENERATED: do not edit manually */}

Generated from `bd help --doc delete`.

Delete one or more issues and clean up all references to them.
This command will:
1. Remove all dependency links (any type, both directions) involving the issues
2. Update text references to "[deleted:ID]" in directly connected issues
3. Permanently delete the issues from the database

This is a destructive operation that cannot be undone. Use with caution.

BATCH DELETION:
Delete multiple issues at once:
  bd delete bd-1 bd-2 bd-3 --force

Delete from file (one ID per line):
  bd delete --from-file deletions.txt --force

Preview before deleting:
  bd delete --from-file deletions.txt --dry-run

DEPENDENCY HANDLING (the same on a local database and against a team server):
Default: Fails if any issue has dependents not in deletion set
  bd delete bd-1 bd-2

Cascade: Recursively delete all dependents
  bd delete bd-1 --cascade --force

Force: Delete and orphan dependents
  bd delete bd-1 --force

Protect active references: Refuse deleting a closed issue while any surviving
issue that refers to it is not closed. This check also applies to --dry-run and
cannot be overridden by --force. Owners included in a cascade are not survivors.
On success, surviving reference owners receive fresh revisions.
  bd delete bd-1 --force --protect-active-references

Use --if-revision with the current issue revision to reject stale deletion.
Guarded deletion is unavailable against a proxied server.

```
bd delete <issue-id> [issue-id...] [flags]
```

**Flags:**

```
      --cascade                     Recursively delete all dependent issues
      --dry-run                     Preview what would be deleted without making changes
  -f, --force                       Actually delete (without this flag, shows preview)
      --from-file string            Read issue IDs from file (one per line)
      --if-revision int             Apply this single-issue write only if its opaque revision matches; mismatch exits 9, unsupported topology exits 13
      --protect-active-references   Refuse deletion of closed issues referenced by surviving active issues, even with --force
```
