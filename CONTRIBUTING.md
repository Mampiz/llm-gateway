# Contributing

## Getting set up

Go 1.26 or newer, and nothing else:

```bash
./scripts/bootstrap.sh
```

It builds, runs both suites, drives the real binary through the smoke checks and
prints a working API key. Docker is optional and only unlocks the tests that
need a real Redis.

## The loop

```bash
make test    # fast, while you work
make smoke   # does it actually work
make ci      # what the pipeline will run
```

`make ci` is the same set of steps as the workflows, so a green run locally is a
green run on the pull request.

## What the tests are for

Three layers, each answering a different question:

- **Unit tests** ask whether a piece behaves. They use `httptest.NewServer`
  rather than a mocked transport, so the whole `net/http` stack is exercised.
- **End-to-end tests** (`-tags e2e`) ask whether the assembled program behaves.
  They compile the real binary, run it on a kernel-assigned port and talk to it
  over a socket.
- **Integration tests** (`-tags integration`) ask whether the Redis-backed parts
  behave. A fake would not exercise the Lua script's atomicity, which is the
  only reason that code exists.

Run everything under `-race` from the first attempt, not at the end.

## House style

- **Comments say why, not what.** The code already says what it does. A comment
  earns its place by recording a decision, a constraint or a trap.
- **Every fix carries the test that proves it.** Write the failing test first;
  a fix without one is a guess.
- **Errors are wrapped with `%w`** and prefixed with the package name. A type
  that carries another error implements `Unwrap`.
- **Table-driven tests** for anything with more than two cases.
- **Assertions name the consequence**, not just the mismatch:
  `want 200: the fallback should have served this` beats `got 500 want 200`.

## Adding a provider

The point of the layout is that this touches no existing file:

1. Create `internal/provider/<vendor>/`.
2. Implement `provider.Provider`: `Name`, `Chat`, `ChatStream`.
3. Keep every vendor type unexported. That vocabulary must not leave the
   package. The core knows none of it, and that is what keeps the next
   provider cheap.
4. Translate both ways, and be explicit about what does not map. Dropping a
   field silently is worse than rejecting it.
5. Register it in `buildRegistry` in `cmd/gateway/main.go` with its model
   prefixes. Routing policy is a deployment decision, so it lives at the
   composition root rather than inside the client.

## Commits

Conventional commits, one line, no body:

```
type(scope): subject
```

- **Types:** `feat`, `fix`, `test`, `ci`, `docs`, `chore`, and `dev` for
  developer tooling such as the fake upstream or the smoke script.
- **Scope** in parentheses when the change sits in one package, omitted when it
  spans several.
- **Imperative and lowercase** after the colon, no trailing period.
- 45 to 80 characters, describing the change rather than the process.

```
feat(openai): implement non-streaming chat completions passthrough
fix(server): abort instead of splicing an error into a started response
ci: split workflows, add e2e suite, linting, security scanning and MIT license
```
