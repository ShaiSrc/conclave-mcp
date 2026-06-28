# Contributing to Conclave MCP

Thanks for your interest. Conclave is a small project and most
contributions are small PRs — a new flag, a docs fix, a test
addition. The project follows a **spec-first workflow**: a
change that adds behaviour starts with a spec, not with code.

## TL;DR

1. Read the spec or ADR for the area you are changing.
2. If the spec does not exist or is unclear, open an issue
   before writing code.
3. Open a topic branch named `topic/short-summary`.
4. One commit per logical change. The commit message
   references the spec / ADR / issue it implements.
5. Run `go test -count=1 -short ./...` before pushing.
6. Open a PR with a description that quotes the spec line you
   are implementing.
7. The PR is reviewed; CI is green; merge.

## The spec-first workflow

Conclave follows the same model that the maintainer uses
internally. It is the same model documented in
[`docs/adr/0007-interagent-audit.md`](docs/adr/0007-interagent-audit.md)
in spirit, even though the maintainer's authoritative specs
live in a private Obsidian vault.

**Rule:** if your change adds or changes **observable**
behaviour, there must be a spec line that says what the
behaviour is. The spec lives in
`/workspace/notes/Conclave/Specs/SPEC-XXXX-...md` (vault,
not in this repo) and is referenced from the commit message
and the PR description.

For external contributors, this is a problem: you cannot
edit the maintainer's vault. Workaround:

* Open an issue with the proposed spec change.
* The maintainer updates the spec.
* You then open the PR that implements it.

The maintainer is happy to take the spec change on faith if
the proposal is small and well-reasoned; the goal is to keep
the spec the source of truth, not to gatekeep every change.

## Branch strategy

* Default branch: `main`.
* Topic branches: `topic/<short-summary>`.
  Examples: `topic/fix-ratelimit-headers`,
  `topic/spec-trust-origin-external-default`,
  `topic/docs-cli-reference`.
* Long-lived branches (release branches, experiments) get
  a different prefix: `release/...`, `experiment/...`.
* Force-pushes to `main` are forbidden.
* Force-pushes to topic branches are tolerated during the
  PR review (squash-and-rebase workflow) but discouraged.

## Commit messages

Format:

```
<type>(<scope>): <subject>

<body>

<footer>
```

Types:

* `feat` — new observable behaviour.
* `fix` — bug fix.
* `docs` — documentation only.
* `refactor` — internal change, no behaviour diff.
* `test` — adding or fixing tests.
* `chore` — build, deps, CI, housekeeping.

Scope: the package or area, in kebab-case. Examples:
`(messages)`, `(cli)`, `(mcp)`, `(audit)`, `(admin)`,
`(server)`, `(events)`, `(storage)`, `(auth)`, `(docs)`,
`(deps)`, `(ci)`.

Subject: imperative, lowercase, no period, ≤ 72 chars.
Examples: `add --room-id flag to audit list`,
`expose trust_origin on cli and mcp surfaces`,
`add admin keys create-bootstrap subcommand`.

Body: wrap at 72 chars. Explain **why**, not what. Quote
the spec line you are implementing. If the commit is a
bug fix, describe the failure mode. If the commit is a
refactor, describe the maintenance burden you are removing.

Footer: `Refs: SPEC-XXXX §X.Y`, `Refs: ADR-XXXX`,
`Refs: #NN` (issue or PR). One per line.

Example:

```
feat(admin): add 'admin keys create-bootstrap' subcommand

The first-use setup for a fresh Conclave install was
to write a Go script (or use the test helper in
tests/e2e/helpers_test.go) that imports internal/auth
and inserts a row directly into the api_keys table.

After this commit, the operator runs:

  $ conclave-mcp admin keys create-bootstrap \
      --label "first-operator" \
      --trust-origin=internal

Refs: SPEC-0004 §5.1, ADR-0007 §Open questions
```

## Tests

Conclave has three layers of tests:

* **Unit tests** in the package they cover (`internal/foo/foo_test.go`).
  Use the `testutil` package for in-memory SQLite and HTTP
  test doubles.
* **Integration tests** in `tests/e2e/` that spin up a real
  server against a real SQLite file and exercise the HTTP API.
* **Security tests** in `internal/security/`.

The acceptance criterion for a PR is "`go test -count=1
-short ./...` exits 0 and the new test is part of the suite".
If you cannot write a unit test for your change (because the
behaviour is only observable at the e2e level), the criterion
is "`tests/e2e/...` exits 0 with the new test in the run".

**Hard rule:** "tests added" is **not** an acceptance
criterion. "go test -count=1 -short ./... returns 0" is.

## Code style

* `gofmt -s` and `goimports` on save.
* `go vet ./...` clean.
* No `// nolint` without an ADR-XXXX reference.
* No init() in production code. Migrations run from a
  single function in `internal/storage/migrate.go`.
* No global mutable state. The broker, config, and DB pool
  are explicit dependencies of the server constructor.
* Errors: wrap with `fmt.Errorf("...: %w", err)`. The
  application layer distinguishes `client.NetError`,
  `client.APIError`, and validation errors at the HTTP
  boundary (see `internal/cli/client`).

## Pull request workflow

1. The PR description quotes the spec line that is being
   implemented, plus a short note on how the test exercises it.
2. CI is green. CI runs: `go vet`, `go test -count=1 -short`,
   `go test -race`, `gofmt -l`, `markdownlint`.
3. The maintainer reviews. The review focuses on:
   * spec alignment (does this implement what the spec says?),
   * test coverage (is the new behaviour tested?),
   * drift (does this change the wire format in a way the
     changelog needs to mention?).
4. Squash-and-merge by default. The squash commit message
   is the maintainer's responsibility; it should reference
   the spec or ADR if the original commits did.
5. After merge, the topic branch is deleted.

## What needs a spec / ADR?

* Adding a CLI flag, an HTTP endpoint, or an MCP tool: spec
  change in SPEC-0003 or SPEC-0004.
* Changing the wire format: spec change in SPEC-0002 or
  SPEC-0003 plus a migration in `migrations/`.
* Changing the auth model, the rate-limit formula, the
  storage engine, or the trust model: ADR required. The
  ADR explains the options, the decision, and the
  consequences. The format is in
  [`docs/adr/README.md`](docs/adr/README.md).
* Documentation-only changes: no spec needed, but the
  commit message references the doc that is being updated.
* Refactors that do not change behaviour: no spec needed,
  but a test must exist that pins the behaviour you are
  preserving.

## Code of conduct

Be kind. The project is small and the maintainer is one
person. Disagreements about design choices are settled in
the spec or ADR, not in the PR comments.
