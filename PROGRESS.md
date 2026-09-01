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

### F1 · Anthropic streaming — closed

Implemented `anthropic.ChatStream` and its iterator in a new `stream.go`,
mirroring the OpenAI one. Phase 3 is now complete: both vendors stream.

**Decisions**

- **Dispatch on the payload's `type` field, not the `event:` line.** The
  vendor sends both and they always agree, but keying off the JSON means one
  parser instead of two and no chance of the two drifting apart.
- **Metadata is stitched across events.** `message_start` carries the id, the
  model and `input_tokens`; `message_delta` carries the stop reason and
  `output_tokens`; the total exists in neither. The iterator holds that state
  between calls, which is why it has more fields than the OpenAI one.
- **`message_stop` is the clean end.** There is no `[DONE]` here, so a stream
  that ends without it is reported as truncated rather than complete.
- **A mid-stream `error` event becomes `Stream.Err`.** The vendor reports late
  failures inside a 200 response; surfacing them any other way would let a
  broken answer look finished.
- **Non-text deltas are dropped.** `input_json_delta` and friends belong to
  tool use, which the canonical schema does not model yet.
- **Extracted `mapStopReason`** out of `fromAnthropic` so the buffered and the
  streaming paths share one mapping table instead of two copies that could
  drift. Behaviour unchanged; the existing tests cover it.

**Verifier** — passing.

## Discarded ideas

_Out-of-scope thoughts land here instead of in the code._
