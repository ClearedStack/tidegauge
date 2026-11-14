<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Tidegauge" height="56">
  </picture>
</p>

<p align="center">
  <strong>Watches official sources, live tools and linked tools for change, and reopens whatever is affected.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy.** All five deterministic watchers, the Muster contract v1 client and example replay suite, the `/var/spool/cs` local spool, the SSRF-safe HTTPS fetcher, and hardened Kubernetes manifests in [`deploy/`](deploy) build and pass `go vet ./...` and `go test -race ./...` using only the Go standard library. See [What Tidegauge needs from other components](#what-tidegauge-needs-from-other-components) for upstream contract items before cluster rollout.

Tidegauge is stage 09 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Tidegauge can run inside the cluster against Muster or standalone from the command line against saved files or its built-in Muster stand-in.

## What works today

### 1. Five deterministic watchers (`tools/`)

Every watcher listed in [`component.json`](component.json) and [`tools/registry.yaml`](tools/registry.yaml) is built and tested against synthetic golden fixtures:

- **`ecfr`** ([`tools/ecfr/`](tools/ecfr)): compares Electronic Code of Federal Regulations XML snapshots via HTTP ETag and two-pass streaming division diffs, extracting changed divisions, paragraph markers, dollar amounts, ISO dates and `[Reserved]` repeals.
- **`federal-register`** ([`tools/federal-register/`](tools/federal-register)): compares Federal Register JSON API responses and `FEDREG` XML feeds for rulemakings, notices, action classifications (`final_rule`, `interim_rule`, `proposed_rule`, `withdrawal`, `repeal`), CFR references, RINs, effective dates and dollar thresholds.
- **`nist-csrc`** ([`tools/nist-csrc/`](tools/nist-csrc)): compares NIST CSRC Atom 1.0 feeds, RSS 2.0 feeds and publication HTML DOM pages for publication revisions, errata dates and `withdrawn`/`superseded` lifecycle transitions.
- **`linked-tool-watch`** ([`tools/linked-tool-watch/`](tools/linked-tool-watch)): inspects external `linked` tools for HTTP `404`/`410` dead links (`offline`), HTTP `402` or JSON-LD/DOM paywalls (`paywall`), and added analytics/telemetry scripts (`tracking_script`) without retaining external HTML, prose or hostnames.
- **`live-catalog`** ([`tools/live-catalog/`](tools/live-catalog)): checks published `live` ClearedStack manifests and `cs:*` HTML metadata for `health_failed`, `rule_version_drift` and `repealed` rules.

Run any watcher directly on two saved files:

```sh
go run ./cmd/tidegauge ecfr \
  -old tools/ecfr/testdata/changed-threshold/old.xml \
  -new tools/ecfr/testdata/changed-threshold/new.xml
```

Exit status is `0` when the check ran (`changed` or `unchanged`), `1` when it ended in one of the three error codes (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`), and `2` for a usage mistake.

### 2. Continuous Muster worker (`tidegauge work`)

In cluster mode, `tidegauge work` authenticates to Muster with its projected Kubernetes ServiceAccount token (`/var/run/secrets/kubernetes.io/serviceaccount/token`) and speaks Muster contract v1 (`POST /v1/lease`, `/v1/heartbeat`, `/v1/complete`, `/v1/fail`, `/v1/needs_subtool`):

- **Local spool (`/var/spool/cs`):** every completed check is atomically written and synced to `/var/spool/cs/outbox` before calling Muster and drained in FIFO order across Muster restarts. Large upstream XML payloads spill to `/var/spool/cs/tmp` so Go heap usage stays within the `30 MiB` pod limit (`GOMEMLIMIT=24MiB`).
- **Source-text-free baselines:** compact digests and ETags are returned in `results.tidegauge` (well within Muster's `64 KiB` limit) so 7-day revisit checks send `If-None-Match` and detect drift without touching Logbook directly.
- **Retirement handoff:** when a governing clause or standard is repealed or superseded (`live -> retired`), `results.tidegauge` includes the `retire` object (`reason` and `note`) expected by Chartroom.

Run a self-contained worker cycle without a cluster using the built-in Muster stand-in:

```sh
go run ./cmd/tidegauge work -standin -once -spool /tmp/tidegauge-spool
```

### 3. Tests

```sh
go vet ./...
go test -race ./...
```

The test suite requires Go 1.24 or later and zero external packages, network access or cluster credentials. [`internal/muster/contract_test.go`](internal/muster/contract_test.go) replays all 52 saved examples from `ClearedStack/muster`'s `contract/v1/examples/` directory (vendored verbatim under [`testdata/contract/v1/examples/`](testdata/contract/v1/examples)).

## What Tidegauge needs from other components

Tidegauge does not invent endpoints, fields or workarounds outside Muster contract v1. Three upstream items are needed for every transition in [`SPEC.md`](SPEC.md) to run in production:

1. **Pausing `live`, `linked` and `parked` items in Muster (`ClearedStack/muster`):** `CONTRACT.md` ("Item states and moves") currently allows `-> needs_subtool` only from `recorded`, `observed` and `approved`. When an upstream regulatory feed changes layout on a `live` item, Tidegauge calls `POST /v1/needs_subtool` as required; Muster currently refuses that move with `409 move_not_allowed` until `live`, `linked` and `parked` are added to Muster's pausable states.
2. **Queue steps for `linked` and `parked` revisits (`ClearedStack/muster`):** `CONTRACT.md` ("Queue steps") currently schedules 7-day Tidegauge checks only for `live` items (`Chartroom: building -> live`, `Tidegauge: live -> live|building|retired`). Scheduling `linked` and `parked` revisits requires Muster queue rows for `linked -> linked`, `linked -> observed`, `parked -> parked` and `parked -> observed` (plus queuing Tidegauge after `Chartroom` lists a `linked` item and after `Countersign`/`Shakedown` parks an item).
3. **Shared `results` conventions (`Waybill`, `Chartroom`, `Countersign`, `Shakedown`) and release digest stamping (`Keelworks`):** Tidegauge reads watched URLs and rule versions from `results.chartroom` (`live_url`, `link.url`, `provenance.links`, `rule_version`), `results.waybill` (`sources`, `targets`, `rule_version`) and `results.countersign`/`results.shakedown` (`blocker_url`, `watch_url`). Before applying [`deploy/tidegauge.yaml`](deploy/tidegauge.yaml), Keelworks must stamp the built `lazarette.clearedstack.svc.cluster.local:5000/tidegauge@sha256:...` image digest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Tidegauge does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component's entry in the ClearedStack naming registry. |
| [`.ko.yaml`](.ko.yaml) | Reproducible `ko` image build configuration pinned to `distroless/static-debian12:nonroot` by digest. |
| [`deploy/`](deploy) | Hardened `cs-workers` `ServiceAccount`, `Deployment` and default-deny `NetworkPolicy` manifests. |
| [`cmd/tidegauge/`](cmd/tidegauge) | The command-line entrypoint (`tidegauge work` and the five saved-file watcher subcommands). |
| [`internal/muster/`](internal/muster) | Strict Muster contract v1 HTTP client, stand-in server and 52-example contract replay tests. |
| [`internal/spool/`](internal/spool) | Atomic `/var/spool/cs` outbox and scratch directory manager. |
| [`internal/fetch/`](internal/fetch) | SSRF-safe public HTTPS fetcher with `If-None-Match` ETag support and disk spilling. |
| [`internal/worker/`](internal/worker) | Continuous lease, heartbeat, watcher dispatch and state-transition loop. |
| [`internal/watch/`](internal/watch) | Shared public error codes (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`), outcomes and deterministic text helpers. |
| [`tools/`](tools) | The five deterministic watchers and [`tools/registry.yaml`](tools/registry.yaml). |
| [`testdata/contract/v1/examples/`](testdata/contract/v1/examples) | Exact copy of `ClearedStack/muster`'s `contract/v1/examples/` replayed by `go test`. |
| [`brand/`](brand) | The Tidegauge logo and mark, for light and dark backgrounds. |

## At a glance

| | |
| --- | --- |
| **Stage** | 09 of 10 in the pipeline |
| **Formerly** | `cs-09-monitor`, then Dogwatch |
| **Namespace** | `cs-workers` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/tidegauge` |
| **Memory budget** | 30 MiB |
| **Depends on** | [Muster](https://github.com/ClearedStack/muster) |
| **Depended on by** | Nothing else in the platform |
| **Rules for particular cases** | `federal-register`, `ecfr`, `nist-csrc`, `linked-tool-watch`, `live-catalog` |

### Boundaries

- Leases scheduled checks from Muster using its Kubernetes ServiceAccount token; never touches Logbook directly
- Fetches public Federal Register, eCFR, NIST CSRC, live catalog and linked tool HTTPS endpoints under a default-deny `NetworkPolicy`

## Names

`tidegauge` is this component's identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component's entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
