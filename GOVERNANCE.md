# Governance

## Current state

EffectTrace is a **single-maintainer** open-source project, licensed under the
Apache License 2.0. It is currently maintained by the GitHub user
[@sandeepbazar](https://github.com/sandeepbazar).

EffectTrace is an independent project. It is not part of, endorsed by or
affiliated with any foundation, including the CNCF or the Linux Foundation, and
it has no corporate owner.

## Roles

| Role | Who | Responsibilities |
| --- | --- | --- |
| Maintainer | @sandeepbazar | sets direction, reviews and merges changes, cuts releases, handles security reports, enforces the Code of Conduct |
| Contributor | anyone | opens issues and pull requests, reviews, improves documentation and tests |

## How decisions are made

- **Day-to-day changes** are proposed as pull requests and merged by the
  maintainer after review and green CI.
- **Significant decisions** (anything that changes the evidence model, the
  effect graph schema, storage, privacy defaults, export semantics or security
  posture) are recorded as an [architecture decision record](docs/adr/README.md).
  Proposals start as an issue or a pull request adding a `Proposed` ADR and stay
  open for comment for at least one week before the maintainer decides.
- **Evidence over opinion.** Changes to attribution must be backed by tests and,
  where behaviour on a cluster matters, by an experiment
  ([testing](docs/testing.md)). Results are generated, never hand-edited.
- The maintainer explains the reasoning for decisions in the issue, pull
  request or ADR, including when a proposal is declined.

## Adding maintainers

The project would like more maintainers. A contributor can become a maintainer
when they have:

- made sustained, high-quality contributions over several months (code,
  reviews, tests or documentation);
- shown good judgment about the evidence model and its limits;
- agreed to follow this governance, the [Code of Conduct](CODE_OF_CONDUCT.md)
  and the [security policy](SECURITY.md).

The current maintainers nominate a candidate in a public issue; the nomination
is accepted if no maintainer objects within two weeks. New maintainers are
listed in this file.

Once there are three or more maintainers, significant decisions require
lazy consensus among maintainers, with a simple majority vote if consensus
cannot be reached. This file would then be updated to describe the process in
detail, including how a maintainer steps down or becomes inactive.

## Changes to this document

Changes to governance are made by pull request and follow the process for
significant decisions.
