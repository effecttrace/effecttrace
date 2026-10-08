# Releasing

EffectTrace has not published a release yet. v0.1.0 is not released. This page
describes the process the maintainer follows. Release automation (signed
binaries, images, SBOMs) is not in place yet and is tracked in the
[roadmap](../ROADMAP.md).

## Versioning

- Semantic versioning, with a `v` prefix (`v0.1.0`).
- During v0.x, minor versions may change the effect graph JSON, flags and
  `pkg/` APIs. Such changes are listed in [CHANGELOG.md](../CHANGELOG.md).
- The effect graph has its own schema version (`effecttrace.io/v1alpha1`),
  changed only when the JSON shape changes.
- The binary version is set at build time:
  `make build VERSION=v0.1.0` embeds `VERSION` and the commit.

## Signing policy

- Every commit on `main` is signed (SSH or GPG) and attributed to the
  maintainer's GitHub identity. Unsigned commits are not merged.
- Every release tag is an annotated, **signed** tag. Unsigned tags are not
  releases.
- `make scan` verifies that commit identities, messages and tags contain no
  personal data, credentials or attribution strings.

## Checklist

1. **Branch state.** `main` is green in the `ci`, `codeql` and `e2e`
   workflows.
2. **Verify locally.**

   ```sh
   make verify
   make vuln
   make fuzz FUZZTIME=60s
   ```

3. **Regenerate results on a clean tree.** Results are never edited by hand.

   ```sh
   make demo-up
   make test-e2e          # writes test-results/results.json, environment.json, graphs/
   make benchmark         # writes test-results/benchmarks.json
   make test-results      # summary.json, docs/results.md, docs/benchmarks.md, README block
   make demo-down
   ```

   `results.json` records the commit and whether the tree was dirty. Run from
   a clean checkout of the commit you intend to tag so the published numbers
   refer to it. If any scenario fails or errors, fix it or document it before
   releasing; never remove a failing scenario to make a release pass.
4. **Update documentation.** Move the `[Unreleased]` entries in
   [CHANGELOG.md](../CHANGELOG.md) under the new version with the date, and
   check [upstream compatibility](upstream-compatibility.md) and
   [DEPENDENCIES.md](../DEPENDENCIES.md).
5. **Commit** the regenerated results and documentation as a signed commit.
6. **Tag.**

   ```sh
   git tag -s v0.1.0 -m "EffectTrace v0.1.0"
   git push origin v0.1.0
   ```

7. **Build artifacts** from the tag with `make build VERSION=v0.1.0` (and the
   image build in [deployment](deployment.md#1-build-and-publish-the-image)),
   and attach them to the GitHub release with checksums.
8. **Release notes** link to the CHANGELOG section, [results](results.md) and
   [limitations](limitations.md).

## Security releases

Security fixes follow [SECURITY.md](../SECURITY.md): the fix is prepared
privately, released as a patch version, and the advisory is published with
the release.
