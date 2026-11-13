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

> **Status: design.** Nothing is built yet. This repository holds the specification and the prompt for building it, and no code.

Tidegauge is stage 09 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Tidegauge is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Tidegauge does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
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
