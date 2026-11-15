# sous

An OpenAI-compatible LLM gateway (Go), a single-GPU local inference service
(Python), and a cooking-log Discord bot that runs on top of them.

## Local models

Everything runs on one RTX 5080 (16 GB) that is also used for gaming. Each
model is served by its own llama.cpp `llama-server` process, which speaks the
OpenAI API:

| Role | Model | VRAM | Load (warm / cold) | Generation |
|---|---|---|---|---|
| Extraction | Qwen3.5-4B, Q8_0 | 5.1 GB | 1.8 s / 2.6 s | 134 tokens/s |
| Chat | Qwen3.5-9B, Q8_0 | 9.7 GB | 1.7 s / 4.3 s | 82 tokens/s |
| Embeddings | Qwen3-Embedding-0.6B, Q8_0 | 1.9 GB | 0.75 s / 1.2 s | — |

Together the two chat models do not fit next to what Windows already holds, so
models have to be swapped in and out on demand. The GPU does not refuse an
oversized load: Windows evicts other applications' memory instead, so the
budget has to be enforced by the software. Measurements and findings are in
[docs/models.md](docs/models.md).

## Stage 1: the gateway core and the cooking log (v0.1)

**The gateway** puts one OpenAI-compatible API (`/v1/chat/completions`,
`/v1/embeddings`) in front of several model servers.

- Each caller has its own key and names models only by role: `extract`,
  `chat`, `embed`. The YAML config maps each role to an upstream and to that
  upstream's own name for the model, so swapping a model never touches a
  caller.
- Streamed replies are relayed event by event, flushed as they arrive. If
  the upstream fails mid-stream, the caller gets an error event and a
  cleanly ended response, rather than a dropped connection it cannot tell
  from a network fault.
- Every request gets an ID and one access-log line. Request and response
  bodies and keys never reach the log.
- On SIGTERM it stops taking requests, reports not-ready on `/readyz`, and
  lets requests in flight finish, streams included, within a grace period.

**The cooking log** is a Discord bot for two people who cook at home.
"记 今天做了鱼香茄子，少放了糖，9 分" ("logged: fish-fragrant eggplant
tonight, less sugar, 9/10") becomes a structured record: dish, ingredients,
rating, note, date. The bot shows the record and saves it once the user
confirms. "想吃 豆腐" ("in the mood for tofu") lists every tofu dish cooked
so far, with how often, the average rating and when it was last made.

Key decisions:

- **Forwarding is hand-written, not `httputil.ReverseProxy`.** Later stages
  retry a request on another upstream and count tokens in streams, and
  "send the same request again elsewhere" does not fit ReverseProxy's model.
- **Upstream errors pass through unchanged.** llama-server and
  OpenAI-compatible clouds already share the error shape; rewrapping would
  hide details such as "context length exceeded".
- **The model's output is untrusted input.** The extractor parses strictly:
  unknown fields and anything after the JSON object are errors. It checks
  every field, and sends a failed reply back once with all of its problems
  listed. Gateway errors are not retried there; the gateway will do that
  itself in stage 2. Constrained decoding was left out on purpose, to be
  compared against this approach later.
- **Models at write time, SQL at read time.** The model runs once, when a
  dish is logged; queries never call it. Synonyms such as 西红柿 / 番茄
  (both "tomato") are merged by an alias table, not by asking a model.
- **The boundary is tested, not just intended.** Tests fail if the app
  imports the gateway, or if any upstream address or real model name shows
  up in the app's code or config.

Numbers:

- **Extraction** runs on Qwen3.5-4B through the gateway. It takes 0.35–0.6 s
  per message, and got 8 of 8 test sentences and 6 of 6 corrections right
  (relative dates, missing ratings, inferred ingredients).
- **The 9B model** did no better on the same 12 sentences, at twice the VRAM.
- **Shutdown**: Ctrl+C in the middle of a stream lets the stream finish and
  exits 0; when the grace period runs out, the request is cut off at that
  moment.

## Development

Requires Linux or WSL2 with Go 1.27+, Python 3.12+ and [uv](https://docs.astral.sh/uv/).

```sh
make check   # gofmt, go vet, go test -race, ruff format/lint, pytest
make fmt     # apply the formatters
```
