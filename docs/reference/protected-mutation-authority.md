# Protected mutation authority

`bd serve` can verify short-lived Ed25519 permits for mutations that touch
records explicitly marked as protected. Beads stores the protection class in
its protected-record table. It does not infer protection from issue fields,
metadata, descriptions, or request payloads.

When no authority file is selected, the server does not advertise
`issues.protectedMutation` and protected mutations remain refused. To enable
verification, the service supervisor sets
`BEADS_SERVE_PROTECTED_MUTATION_AUTHORITY_FILE` to an absolute path in a
host-mounted directory. The variable is a server-startup input; workspace,
request, and issue data never select the authority. If the variable is set,
`bd serve` refuses startup when any file, binding, key, signed snapshot, or
schema prerequisite is unavailable or invalid.

The authority directory and its files must be regular, non-symlinked objects
that are not writable by group or other users. The directory must not be
writable by group or other users. All three files live in the same directory.
The configured keyring and revocation names are plain file names, never paths.
The configuration file is pinned for the life of the server. The keyring and
signed snapshot are reread for each protected mutation, so a key removal,
revocation, expiry, or unreadable update immediately fails closed. No network
request is made by verification.

## Authority configuration

The selected file is a strict JSON object with these fields:

```json
{
  "schema_version": 1,
  "audience": "automation-service-a",
  "project_id": "project-uuid",
  "database": "beads",
  "keyring_file": "protected-mutation-keyring.json",
  "revocations_file": "protected-mutation-revocations.json"
}
```

The audience, project ID, and database are separate required values. At
startup, Beads compares project ID and database against the selected
`ContextInfo` snapshot, including the database selected by `bd serve`. It does
not derive audience from either value. Duplicate or unknown JSON members are
rejected.

The keyring file is:

```json
{
  "schema_version": 1,
  "generation": 7,
  "keys": [
    {
      "key_id": "permit-2026-01",
      "issuer": "automation-service-a",
      "purpose": "protected_mutation_permit",
      "public_key": "STANDARD_BASE64_ED25519_PUBLIC_KEY"
    },
    {
      "key_id": "revocations-2026-01",
      "issuer": "automation-service-a",
      "purpose": "protected_mutation_revocation",
      "public_key": "STANDARD_BASE64_ED25519_PUBLIC_KEY"
    }
  ]
}
```

There must be at least one key for each purpose. Key IDs and public key
material are unique. Permit and revocation keys use separate purposes and
separate Ed25519 key material. Increase `generation` whenever key membership
or purpose assignments change.

## Permit format and signature

A permit is two unpadded raw-URL-base64 values separated by one period:

```text
base64url(canonical_payload) . base64url(ed25519_signature)
```

The signature input is the UTF-8 bytes of
`beads.protected-mutation-permit.v1\n` followed by the canonical payload bytes.
The payload is a strict JSON object with fields in this order:

```json
{
  "schema_version": "beads.protected-mutation-permit.v1",
  "purpose": "protected_mutation_permit",
  "key_id": "permit-2026-01",
  "issuer": "automation-service-a",
  "audience": "automation-service-a",
  "project_id": "project-uuid",
  "database": "beads",
  "operation": "issue.update",
  "resource_ids": ["bd-1", "bd-2"],
  "request_digest": "64-lowercase-hex-characters",
  "replay_id": "permit-use-00000001",
  "issued_at": "2026-09-28T12:00:00Z",
  "expires_at": "2026-09-28T12:01:00Z"
}
```

Canonical payload bytes are the compact UTF-8 JSON encoding of the listed
fields in that order, with no whitespace outside strings. Strings use the
JSON escaping rules used by Go's `encoding/json` (including escapes for `<`,
`>`, `&`, U+2028, and U+2029). Times use their exact RFC 3339 Nano spelling.
Resource IDs are unique, nonempty, and sorted by UTF-8 byte order. The request digest is the
lowercase hexadecimal SHA-256 digest that Beads computes with
`issueops.ProtectedMutationDigest`. The replay ID is 16 to 200 ASCII
characters: the first character is a letter or digit; later characters may
also be `.`, `_`, `:`, or `-`.

The verifier fixes the maximum permit lifetime at five minutes and allows 30
seconds of clock skew. The signed issuer must equal the issuer assigned to the
selected key. Every claim must match the server's configured audience,
project/database, operation, canonical resource IDs, and request digest.

## Signed revocation snapshot

The revocation file is a strict JSON envelope:

```json
{
  "payload": {
    "schema_version": "beads.protected-mutation-revocations.v1",
    "purpose": "protected_mutation_revocation",
    "key_id": "revocations-2026-01",
    "issuer": "automation-service-a",
    "audience": "automation-service-a",
    "project_id": "project-uuid",
    "database": "beads",
    "generation": 7,
    "issued_at": "2026-09-28T12:00:00Z",
    "expires_at": "2026-09-28T12:05:00Z",
    "revoked_replay_ids": ["permit-use-00000000"]
  },
  "signature": "UNPADDED_RAW_URL_BASE64_SIGNATURE"
}
```

The revocation signature covers the compact canonical payload bytes in the
shown member order, prefixed by
`beads.protected-mutation-revocations.v1\n`. The snapshot's audience and
project/database must match the authority configuration. Its generation must
equal the keyring generation, so files from different rotations cannot be
combined. The snapshot must be current, no older than five minutes, and valid
within the same 30-second clock skew. Revoked replay IDs use the same syntax
as permit replay IDs and are unique and sorted.

The host supervisor should replace the keyring and snapshot together and keep
their generation equal. A partial update causes verification to fail closed.
The database records each accepted `(audience, replay_id)` in the same
transaction as the protected mutation, so retrying a consumed permit cannot
apply a second mutation.
