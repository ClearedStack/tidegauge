# Tidegauge specification

> Scheduled monitoring worker that watches FAR/DFARS/NIST feeds, live tools, linked tools, and parked blockers for deterministic drift and reopens affected items.

**Status: design.** This is a specification. It describes how Tidegauge is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/tidegauge/](https://clearedstack.com/platform/tidegauge/). To change the specification, change it here and say why.

## Overview & Role

Tidegauge is the continuous monitoring worker in the `cs-workers` namespace (`ownerRepo: Tidegauge`, version `v0.1.0-draft`). Replacing the original Stage 10 (Revisit), it watches federal regulatory feeds, `live` ClearedStack tools, external `linked` tools, and `parked` blockers for drift.

Tidegauge is a 100% deterministic Go service with zero AI at runtime. Rather than scraping pages blindly or patching tools directly, it uses lightweight HTTP ETags, structured XML/API feeds, and deterministic DOM diffs—and reopens affected `PA-XXXX` items in Muster so the main pipeline handles updates cleanly.

## Runtime & Resource Budget

| Property | Value |
| --- | --- |
| **Service / Repo** | Tidegauge (`tidegauge`) |
| **Namespace** | `cs-workers` |
| **Kind** | `worker` (100% deterministic Go, zero runtime AI) |
| **Version** | `v0.1.0-draft` |
| **RAM Budget** | `30 MiB` |
| **Sub-Tools** | `federal-register`, `ecfr`, `nist-csrc`, `linked-tool-watch`, `live-catalog` |
| **Local Spool** | `/var/spool/cs` (buffers detected drift events if Muster is restarting) |
| **Contract Version** | `CONTRACT_VERSION=v1` (owned by `clearedstack.com`) |

Tidegauge runs under the Standard Kubernetes priority class with a `30 MiB` memory footprint, yielding before High-priority control-plane pods on a single 4 GiB `k3s` node.

## Interfaces & State Transitions

Tidegauge pulls scheduled watch tasks from Muster over cluster-private HTTP using its Kubernetes ServiceAccount identity (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`). It never touches Logbook directly.

When a deterministic diff confirms a meaningful change, Tidegauge stages the finding in `/var/spool/cs` and requests a state transition through Muster:

| Current State | Reopened Target State | Priority Lane | Condition |
| --- | --- | --- | --- |
| `live` | `building` | `urgent` | Governing regulation shifted or live tool rule version drifted; preempts normal/backfill work for an updated release. |
| `live` | `retired` | `urgent` | Underlying statutory rule or clause was repealed or superseded. |
| `parked` | `observed` | `normal` | Previously named blocker (e.g., pending rule or missing public data) cleared on scheduled revisit. |
| `linked` | `observed` | `normal` | External free tool went offline, added a paywall, or introduced tracking scripts; returned for clean-room build decision. |

## Sub-Tools & Failure Modes

Each source type is implemented as a deterministic sub-tool under `tools/<case>/` inside the Tidegauge repository, paired with golden test fixtures:

| Sub-Tool | Target Monitored | Detection Method |
| --- | --- | --- |
| `federal-register` | Federal Register notices and rulemakings | Scheduled XML and API diffs for FAR, DFARS, and small-business rule updates. |
| `ecfr` | Electronic Code of Federal Regulations | HTTP ETag and XML diffs tracking modified clauses, thresholds, and effective dates. |
| `nist-csrc` | NIST Computer Security Resource Center | Scheduled feed and DOM diffs for cybersecurity standards such as SP 800-171. |
| `linked-tool-watch` | External tools marked `linked` in the catalog | Periodic HTTP and DOM checks for dead links, new paywalls, or added tracking scripts. |
| `live-catalog` | ClearedStack tools published as `live` | Scheduled health and rule-version checks against active catalog entries. |

| Error Code | Trigger | Pipeline Behavior |
| --- | --- | --- |
| `E_TIMEOUT` | Upstream government feed or external linked site fails to respond in time | Item stays safe in queue for retry; repeated timeouts are deduplicated by Bellbuoy. |
| `E_PARSE` | Corrupt XML/JSON feed or broken HTTP response | Refuses to guess; logs sanitized failure and halts that check cleanly. |
| `E_NEEDS_SUBTOOL` | Upstream feed or DOM layout changes to an unrecognized structure | Fails closed, pauses item in `needs_subtool`, and triggers Bellbuoy to open a task; auto-replays after Shipkeeper deploys the new watcher. |

## Security & Clean-Room Boundaries

- **Restricted network egress:** Default-deny network policies permit Tidegauge to call Muster and approved public regulatory/catalog targets only; Logbook, Plainforge, and Lazarette are unreachable.
- **Unprivileged pod posture:** Runs as non-root with dropped Linux capabilities, no privilege escalation, and a read-only root filesystem (apart from `/var/spool/cs`).
- **Clean-room separation:** Tidegauge only inspects public regulatory feeds and catalog metadata; it never injects raw external HTML into Moldloft payloads (Muster rejects any such attempt).
- **Sanitized status:** Internal cluster details are stripped by Shipkeeper before any `E_TIMEOUT`, `E_PARSE`, or `E_NEEDS_SUBTOOL` alert is surfaced publicly.

## Related pages

- [The pipeline and its ten workers](https://clearedstack.com/platform/pipeline/)
- [An item’s life: fourteen states and three lanes](https://clearedstack.com/platform/item-lifecycle/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
