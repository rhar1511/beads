# Owner-fork module release

The owner-fork Go module is `github.com/rhar1511/beads`. Version `v1.3.2` is
prepared on the isolated `inktree-x8mk6` packaging branch from accepted fork
merge `a3ec8915b8c6d5927e610010140eb4a478976c77`. It is a candidate until its
reviewed release branch, immutable tag and public proxy content are verified.

## Distribution scope

Publish only to `rhar1511/beads`: the Go module tag plus a GitHub source
release with exact commit/tree, migration manifest, checksums and qualification
provenance. Do not execute `.goreleaser.yml` or the upstream release molecule:
they belong to `gastownhall/beads` and its upstream npm, PyPI, Homebrew and
marketplace channels. Their existing repository guards and installer fallbacks
remain unchanged. Synchronized component version files permit normal repository
validation; they do not grant or claim publication to those channels.

## Consumer migration

After the real tag is published and independently resolves through the normal
Go proxy and checksum database, a consumer may use:

```bash
go get github.com/rhar1511/beads@v1.3.2
```

Update its Go imports and generated dependency labels to this module. Do not
redirect the upstream module with a replacement. Equal or newer displayed CLI
versions from a different distribution are not compatibility proof: bind the
actual library module/ref/tree/catalog and executed CLI source/artifact, then
qualify their native schema, writer and SourceSnapshot boundaries.

The nested examples retain their explicit local-development replacement to
this checkout; they are separate modules and are not independently released by
the root tag. Use their documented local build instructions.

## Qualification and immutable publication

1. Preserve all historical SQL migrations, protected/CAS/deletion semantics
   and accepted schema cursors 73/28. This packaging is not a migration of any
   production database.
2. Regenerate the OpenAPI bindings with the pinned Go tool and verify active
   package selectors, scanners, lint boundaries and nested examples.
3. Pass the normal version checks, zero-finding lint, complete Go baseline,
   upgrade and regression gates, npm and MCP package gates, release-target
   cross-compilation and the fork's source-qualified Main/Nightly gates.
   Retain real skips and platform execution limits honestly.
4. Independently review the full packaging diff, cut the release branch from
   qualified source, and land release changes through the normal PR flow.
   Preserve every upstream publishing guard. Source-only fork publication
   does not turn skipped canonical publishing jobs into qualification.
5. Immediately before publishing, prove the chosen stable tag is still absent.
   Never reuse, move or repair a burned tag. Bind the actual accepted commit
   and tree, and publish only the owner-fork source/provenance scope above.
6. Verify the actual tag object and peeled commit/tree, authenticated module
   zip/mod sums, canonical content from that exact Git source and a neutral
   consumer without a replacement or workspace. A version string or checksum
   without the source binding is insufficient.
7. Complete the release documentation cutover described in `RELEASING.md`.
   The existing CLI docs pin deliberately describes an earlier published
   release during preparation. After the real tag exists, update
   `docs/cli-docs.pin` to `v1.3.2` and the release line in `docs/index.md` to the
   owner-fork version and its `rhar1511/beads` release-notes URL. Regenerate
   the CLI corpus with the default pinned generator, then run its `--check`
   and the CLI flag/coverage checks with the pin enabled. Bind any cached
   emitter to that exact tag's source. Land the pin, homepage and generated
   files together through the normal main forward-port PR before declaring
   release acceptance complete; this does not publish the upstream docs site.

Before tagging, an isolated preview may use `BD_DOCS_IGNORE_PIN=1` with an
explicit candidate binary built from the exact source using `CGO_ENABLED=0`
and `-tags=gms_pure_go`. Generate and check that preview separately, retaining
its source and binary hashes. Preview evidence does not replace the post-tag,
pin-enabled checks. Do not create a stand-in tag or present a preview as
published-release documentation.

Binary rollout, production configuration and ordinary worker admission are
separate downstream operations. Preserve original source custody and manual
holds until their exact qualification and rollout gates pass.
