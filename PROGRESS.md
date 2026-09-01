# Progress

## Done before this plan

- **Phase 1 · Basic proxy.** Single endpoint forwarding to OpenAI, full HTTP
  cycle, error taxonomy with `Retryable`, graceful shutdown.
- **Phase 2 · Multi-provider normalization.** Anthropic client, canonical
  schema, anti-corruption layer per vendor, model-prefix routing, vendor
  passthrough for unmodelled fields.
- **Phase 3 · Streaming (partial).** SSE reader for OpenAI as an iterator;
  handler multiplexes chunks, heartbeats, idle timeout and cancellation with
  a goroutine, a channel and `select`. Anthropic streaming still missing.
- **Infrastructure.** 6 CI workflows, e2e suite, smoke script, fake upstream,
  10 MB scratch image, release pipeline with SBOM, security scanning.

## Log

_Appended as phases close._

## Discarded ideas

_Out-of-scope thoughts land here instead of in the code._
