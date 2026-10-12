# Conditional issue writes

Read an issue with `bd show <id> --json` and retain its opaque decimal
`revision`. Pass it unchanged to an ordinary single-issue write:

```sh
bd update bd-123 --title "Revised title" --if-revision "$revision" --json
bd assign bd-123 alice --if-revision "$revision" --json
bd close bd-123 --if-revision "$revision" --json
bd delete bd-123 --force --if-revision "$revision" --json
```

The existing lifecycle or delete transaction compares the revision before
writing. A stale revision refuses even a no-op or a forced write. A matching
no-op keeps the revision; a changed aggregate receives a new token. Omitting
the flag preserves the ordinary command behavior; explicitly passing `0` is
a precondition. Tokens are signed 64-bit decimal values, not counters to
increment or timestamps to interpret.

The flag requires one explicit issue ID and a local store without configured
Dolt remotes. Proxied stores refuse before opening their provider. Unsupported
topology never falls back to an unguarded write. Guarded `update` does not
accept `--claim`, label edits or parent edits; use their existing typed roles
for compound operations. Guarded `delete` requires its existing `--force`
confirmation and refuses `--dry-run`, `--cascade` and `--from-file`.

Close follow-ups retain their own workflow transactions. A stale close starts
no follow-up. `--claim-next` earns a claim only when the matching close changes
the issue; an already-closed no-op earns none. A later claim failure reports
that the close succeeded, rather than claiming the entire command made no
mutation. Unguarded batch close and claim remain one transaction.

With `--json`, a revision mismatch exits **9** and returns
`code: "precondition_failed"`, `id`, numeric `expected_revision` and
`current_revision`, and `mutates: false`. Both values come from the failed
transaction and preserve signed 64-bit precision in flat and wrapped JSON.
Detail reads continue to expose `revision` as a string.

Unsupported topology or capability exits **13** with
`code: "conditional_write_unsupported"` and `mutates: false`. A policy refusal,
including a protected issue or another actor's live claim, exits **13** with
`code: "conditional_write_refused"` and `mutates: false`; this does not mean the
backend lacks revision support. These refusals omit mismatch token fields.
`--force` retains its existing policy scope and cannot bypass revision or
protected-record checks. Argument and operational errors retain their ordinary
error behavior and do not invent a successful precondition check.

The command spelling and mismatch/refusal vocabulary build on the contributor
contract in Beads #4682. These adapters use the existing typed transactional
roles rather than introducing another conditional-write engine.
