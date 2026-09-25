# Contributing to vantage

Thanks for looking. This file covers the legal bit, how to get a working
environment, and what this project expects of a change — the last of which
is more opinionated than most, so it is worth skimming before you write
code.

## License and the CLA

vantage is **GPLv3** (`LICENSE`). Contributions are accepted under a
**Contributor License Agreement** (`CLA.md`).

When you open a pull request, a bot asks you to reply with one line agreeing
to `CLA.md`. That covers every contribution you make from then on. You keep your copyright — it is a
license grant, not an assignment.

**Why a CLA, stated plainly rather than left to be guessed at:** it lets the
project offer a commercial license to organizations that cannot accept the
GPL. That is only possible if the project holds the rights to all of the
code, including yours. If that arrangement is not something you want to be
part of, that is a completely reasonable position and we would rather you
knew before writing a patch than after.

If you are contributing on company time or with company equipment, your
employer probably owns the work and will need to sign a Corporate CLA. Open
an issue and we will sort it out.

## Getting a working environment

```
docker compose -f docker-compose.dev.yml up --build -d
```

That brings up NATS, two collectors, ClickHouse, the writer, the API and
Grafana. The second collector (`collector2`, BMP on 11119, metrics on 9569)
watches the same routers as the first (BMP on 11019, metrics on 9469), so
the stack is dual-homed the way a redundant deployment is: point a BMP
sender at both and every per-collector view has two collectors to tell
apart. `docker-compose.dev.yml` explains how to aim a sender at the second
one without the NAT trap. **Everything except the collectors' BMP ports binds
to `127.0.0.1` deliberately** — the dev stack runs Grafana with anonymous
access against a ClickHouse datasource, so anyone who can reach it can run
arbitrary SQL. That is acceptable for a machine you control and is not a
deployment target. Do not move those bindings. The collectors listen on
every interface because routers have to reach them.

The UI runs separately:

```
cd ui && npm install && npm run dev
```

Tests, through the same make targets CI's gate is built from (`make help`
lists them all):

```
make check     # go build, go vet and the Go unit tests, in one line of output
make test      # the full Go gate: race detector AND a live ClickHouse
make ui-test   # the UI suite
make standalone-check  # no published file cites an internal path, a commit SHA or process material
```

`make check` works without the dev stack, but every test that needs
ClickHouse skips there, and its summary line says how many did. `make test`
refuses to skip: it fails if ClickHouse is not reachable on
`VANTAGE_CH_NATIVE_PORT` (default 9000), which is what CI does too.
It also runs `make chart-deps` first, which needs `helm` and fetches the
chart's NATS dependency on the first run; the Helm-rendering tests use it.

`make standalone-check` needs nothing running. It fails on a published file
that cites a path outside the public repo, a commit SHA, or internal process
material such as a numbered task, a review round or a report file; the
patterns, each commented with what it catches, are in
`scripts/citation-patterns.txt`. It scans committed files only, so `git add`
a new file before running it. If it flags a sentence that is ordinary prose,
reword the sentence, or add a `path:regex` entry to
`scripts/citation-allow.txt` that matches that one sentence.
`RACE=1 make check` adds the race detector without requiring the database.

## What a change is expected to look like

This project has a few habits that are not negotiable, because each exists
because of a specific bug that shipped.

**Tests come first, and you must watch them fail.** A test written after the
code passes immediately, which proves nothing about whether it can catch the
bug it names. If you did not see it red, you do not know it works.

**Prove your test can fail.** Break the code deliberately — revert the fix,
flip the comparison, delete the clause — and confirm the test goes red for
the right reason. This project has repeatedly shipped tests that could not
fail, and reading them never caught it. Mutation is the only thing that
does. If a fixture exercises only one branch, it cannot falsify anything:
ask which row takes the other side, and add it.

**Fixtures are captured, never hand-written.** Run the real endpoint against
the real stack and save what it returns. A hand-written fixture encodes what
you believed the shape was, which is exactly the thing under test. There is
a column guard that will reject a field no captured shape carries.

**Numbers about the network must not scale with observers.** Two collectors
watching one router report everything twice. A count of routers, sessions or
paths must not grow because a second collector was watching; a count of rows
archived legitimately does. `ui/src/lib/bestVantage.ts` carries the doctrine and
the reasoning. This defect has shipped eighteen times in this repository.

**Absence is not zero.** "0 routes" and "we have no data" are different
claims, and conflating them is the failure this whole project exists to
prevent. If an answer is unknown, say unknown.

**Say what you measured, not what you assumed.** Commit messages here are
long on purpose: they record what was probed, what the number actually was,
and what was ruled out. "Should be faster" is not a claim; "5.8× on the
shipped path, re-measured after the first number came from the table rather
than the query" is.

## Commit messages

One coherent change per commit, with a subject line that says what changed
rather than which files moved. The body explains *why*, including what you
ruled out and what you measured. If you found something surprising on the
way, write it down — that is usually the most valuable part.

## Reporting bugs

Include what you observed and what you expected, the vendor and OS version
of the router if it is protocol-related, and a packet capture if you can
share one. BMP quirks are extremely vendor-specific; `docs/quirks.md`
records the ones already known.

## Security

Do not open a public issue for a security problem. See `SECURITY.md`.
