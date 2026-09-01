# Commit style

Deduced from `git log -40` over the 13 commits in this repository. The pattern
is fully consistent; every rule below holds for every commit without exception.

## Format

```
type(scope): subject
```

- **Conventional commits.** `type` always present, lowercase, followed by a
  colon and one space.
- **Types observed:** `feat`, `ci`, `chore`, `test`, `dev`. Note `dev` is not
  standard Conventional Commits but is used here for developer-tooling work
  (fake upstream, smoke script). `fix` has not appeared yet; use it for bug
  fixes.
- **Scope optional, in parentheses.** Observed: `(server)`, `(openai)`. Used
  when the change is confined to one package; omitted when it spans several.
- **Imperative mood**, present tense: "add", "stream", "route", "bump".
- **Lowercase** after the colon. No capital initial.
- **No trailing period.**
- **English.**
- **Subject length:** 45-80 characters. Descriptive, not terse.
- **Single line. No body.** All 13 commits are subject-only.
- **Granularity:** one commit per completed unit of work, typically a whole
  phase or a coherent slice of one. Not micro-commits.

## Authorship

`Mampiz <josepmampel20@gmail.com>`. No co-author trailers, no tool
attribution, no mention of assistance. The message describes the change, never
the process.

## Three real examples from the log

```
feat(openai): implement non-streaming chat completions passthrough
ci: split workflows, add e2e suite, linting, security scanning and MIT license
feat(server): multiplex streaming with heartbeats and an idle timeout
```
