# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub private
vulnerability reporting:

**<https://github.com/effecttrace/effecttrace/security/advisories/new>**

Do not open a public issue, pull request or discussion for a suspected
vulnerability. Include:

- the affected version or commit;
- the component (collector, CLI, `pkg/k8sinstrument`, manifests, lab scripts);
- steps to reproduce, ideally with a minimal recording or OTLP payload;
- the impact you expect.

Remove real identities, Secrets and other sensitive data from anything you
attach.

## What to expect

EffectTrace is maintained by a single maintainer ([GOVERNANCE.md](GOVERNANCE.md)),
so response times are best effort. The maintainer will acknowledge the report
in the advisory, investigate, and agree on a disclosure timeline with you. Fixes
are prepared in a private fork of the advisory, released as a patch version,
and the advisory is published with the release. Reporters are credited unless
they prefer otherwise.

## Supported versions

| Version | Supported |
| --- | --- |
| `main` | yes |
| latest release | yes (none published yet; v0.1.0 is not released) |
| older releases | no |

## Scope

In scope: the EffectTrace collector and CLI, the public packages under `pkg/`,
the manifests under `deploy/`, and the lab and helper scripts.

Examples of relevant issues:

- input that crashes the collector, bypasses validation bounds or exhausts
  memory;
- a way to make a graph show an `ATTRIBUTED` relationship that the evidence
  does not support;
- leakage of data the [privacy](docs/privacy.md) defaults promise to drop;
- terminal, HTML or Graphviz injection through graph content;
- authentication bypass of the API bearer token;
- lab scripts acting on a cluster or kubeconfig context other than the local
  `effecttrace-lab`.

Out of scope: the demo shop simulator and demo actors when used outside the
local lab, vulnerabilities in upstream projects (report those upstream), and
issues that require control of kube-apiserver, the node running the
collector, or the collector's command-line flags.

EffectTrace is an observability system, not an authorization system: a
graph is never an access-control decision. See the
[threat model](docs/threat-model.md) and [security model](docs/security-model.md)
for the documented trust boundaries and residual risks.
