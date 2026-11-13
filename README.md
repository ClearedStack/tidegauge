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

> **Status: design.** The specification is not built. One piece of it works today: the `ecfr` watcher, run by hand against two saved files. Everything else in [`SPEC.md`](SPEC.md) is still to do. See [What works today](#what-works-today).

Tidegauge is stage 09 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Tidegauge is meant to be understood and used without the rest.

## What works today

One watcher, `ecfr`. Give it two saved versions of an eCFR source (the XML the eCFR publishes for a title or any piece of one) and it reports whether the source changed and where.

```sh
go run ./cmd/tidegauge ecfr \
  -old tools/ecfr/testdata/changed-threshold/old.xml \
  -new tools/ecfr/testdata/changed-threshold/new.xml
```

```json
{
  "tool": "ecfr",
  "status": "changed",
  "method": "xml-diff",
  "changes": [
    {
      "where": [
        "PART 901",
        "SUBPART 901.1",
        "SECTION 901.102"
      ],
      "kind": "modified",
      "fields": [
        "text"
      ],
      "paragraphs": [
        {
          "op": "removed",
          "position": 1,
          "marker": "(a)"
        },
        {
          "op": "added",
          "position": 1,
          "marker": "(a)"
        }
      ],
      "amounts_removed": [
        "$12,500"
      ],
      "amounts_added": [
        "$15,000"
      ]
    }
  ]
}
```

What it does:

- **Says where.** Each change names the division (part, subpart, section and so on) that was added, removed or modified, whether its heading or its text changed, and which paragraphs.
- **Pulls out thresholds and dates.** Dollar amounts and calendar dates that left or entered a division are listed, so a moved threshold or effective date stands out.
- **Ignores formatting.** A change in line breaks, spacing, comments or attributes other than a division's number is not a change.
- **Uses the ETag when there is one.** If both versions were saved with the same HTTP ETag (`-old-etag`, `-new-etag`), the source is unchanged and neither file is read.
- **Stops instead of guessing.** Broken XML ends the check with `E_PARSE`. Well-formed XML it does not recognise ends it with `E_NEEDS_SUBTOOL`. Messages are fixed sentences and never quote the source.
- **Keeps no source text.** The report holds division numbers, paragraph markers, amounts and dates, and nothing else from the source.

The exit status is 0 when the check ran (changed or unchanged), 1 when it ended in an error code, 2 for a usage mistake.

### Tests

```sh
go test ./...
```

The tests need Go 1.24 or later and nothing else: no network, no cluster, no other tool. Every rule has a saved example under [`tools/ecfr/testdata/`](tools/ecfr/testdata): two saved versions of a source (`old.xml`, `new.xml`, and `old.etag`, `new.etag` where they matter) and the exact report expected (`want.json`). All of it is made-up data about a "title 99" that does not exist.

### Not built yet

- The other four watchers: `federal-register`, `nist-csrc`, `linked-tool-watch`, `live-catalog`.
- Fetching anything. `ecfr` reads saved files only; it makes no HTTP request, so `E_TIMEOUT` can only come from a slow reader in a test.
- Muster: leasing checks, heartbeats, `complete`, `fail`, `needs_subtool`, and reopening items. There is no Muster interface or stand-in yet.
- The state transitions in the specification (`live` to `building` and the rest). `ecfr` reports what changed; nothing decides yet which items that reopens.
- The spool at `/var/spool/cs`.
- The container image, the Kubernetes manifests and the network policy. Nothing has been built with `ko` or run in a cluster.
- The 30 MiB budget has been checked by hand for `ecfr` only (two made-up 131 MB documents compared in about 15 MB), not in a container and not by a test.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Tidegauge does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`cmd/tidegauge/`](cmd/tidegauge) | The command-line program. |
| [`tools/`](tools) | One folder per watcher, and [`tools/registry.yaml`](tools/registry.yaml), which lists the ones that are built. Today: [`tools/ecfr/`](tools/ecfr). |
| [`internal/watch/`](internal/watch) | What every watcher shares: the three public error codes and the outcome names. |
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

- Leases scheduled checks from Muster
- Fetches the Federal Register, the eCFR and NIST's publications, and checks linked and live tools

## Names

`tidegauge` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
