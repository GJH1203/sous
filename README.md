# sous

An OpenAI-compatible LLM gateway in Go, and a cooking-log Discord bot that
runs on top of it, against models served on one gaming GPU.

- `gateway/`: the gateway. Callers send OpenAI-style requests; it routes,
  limits, times and retries them across model servers.
- `app/`: the cooking log, a Discord bot that talks to models only through
  the gateway.
- `internal/fakeupstream/`: a fake model server with switchable latency and
  failures, which every gateway test runs against. No test needs a GPU.
- `inference/`: a Python inference service for the same GPU. Work in
  progress.

## The gateway

One OpenAI-compatible API, `/v1/chat/completions` and `/v1/embeddings`, in
front of several model servers.

**Routing.** Callers name models only by role: `extract`, `chat`, `embed`.
The YAML config maps each role to an upstream and to that upstream's own name
for the model, so swapping a model never touches a caller. Fields the gateway
does not know reach the upstream unchanged.

**Streaming.** Streamed replies are relayed one whole event at a time and
flushed as each arrives. If the upstream fails mid-stream, the caller gets an
error event and a cleanly ended response, rather than a dropped connection it
cannot tell from a network fault.

**Callers and limits.**
- Each caller has its own key, and its own rate limit: a token bucket with a
  burst and a steady rate. Past it, 429 with a `Retry-After`.
- Each upstream takes at most `max_concurrent` requests at once, as many as
  the engine has slots. The rest wait in the gateway, in a queue with a
  timeout, rather than inside the engine.

**Timeouts, per model**, since an embedding takes milliseconds and a chat
answer with thinking most of a minute:
- *connect*: the TCP connection;
- *first byte*: the first byte of the response body, not the headers. A
  streaming engine sends its headers as soon as the request has a slot, long
  before the model has read the prompt, so a header timeout cannot tell a
  slow model from a fast one;
- *idle*: the longest gap between two events of a stream;
- *total*: the whole request, queue and retries included.

A caller can shorten its own deadline with `X-Timeout-Ms`; the upstream is
told the time left in the same header. Each timeout has its own error code,
and the access log says which one fired.

**Retries.** Connection failures, first-byte timeouts, 429, 502, 503 and 504
are retried, up to 3 attempts, with randomized exponential backoff. A
`Retry-After` is a minimum. Two things are never retried:
- anything after the first byte has gone to the caller, since the caller
  would get a second answer spliced onto the first;
- the caller's own mistakes, such as 400 or 404.

A retry budget per upstream allows retries for at most a fifth of recent
requests, plus 3. When an upstream goes down, it sees about 1.2 times its
normal load, not the 3 times that every request retrying would send it.

**Operations.**
- Every request gets an ID, passed to the upstream, and one access-log line.
  Request and response bodies and keys never reach the log.
- On SIGTERM it stops taking requests, reports not-ready on `/readyz`, and
  lets requests in flight finish, streams included, within a grace period.

## The cooking log

A Discord bot for two people who cook at home.
- **Logging a dish:** "记 今天做了鱼香茄子，少放了糖，9 分" ("logged:
  fish-fragrant eggplant tonight, less sugar, 9/10") becomes a structured
  record: dish, ingredients, rating, note, date. The bot shows the record and
  saves it once the user confirms or corrects it.
- **Searching by ingredient:** "想吃 豆腐" ("in the mood for tofu") lists every
  tofu dish cooked so far, with how often, the average rating and when it was
  last made.

## Design decisions

- **Forwarding does not use `httputil.ReverseProxy`.** Retrying a
  request and holding the response back until its first byte do not fit
  ReverseProxy's model.
- **Upstream errors pass through unchanged.** llama-server and
  OpenAI-compatible clouds already share the error shape; rewrapping would
  hide details such as "context length exceeded".
- **The model's output is untrusted input.** The extractor parses strictly:
  unknown fields and anything after the JSON object are errors. It checks
  every field, and sends a failed reply back once with all of its problems
  listed.
- **Models at write time, SQL at read time.** The model runs once, when a
  dish is logged; queries never call it. Synonyms such as 西红柿 / 番茄
  (both "tomato") are merged by an alias table, not by asking a model.
- **The boundary is tested, not just intended.** Tests fail if the app
  imports the gateway, or if any upstream address or real model name shows
  up in the app's code or config.

## Local models

Everything runs on one RTX 5080 (16 GB) that is also used for gaming. Each
model is served by its own llama.cpp `llama-server` process, which speaks the
OpenAI API:

| Role | Model | VRAM | Load (warm / cold) | Generation |
|---|---|---|---|---|
| Extraction | Qwen3.5-4B, Q8_0 | 5.1 GB | 1.8 s / 2.6 s | 134 tokens/s |
| Chat | Qwen3.5-9B, Q8_0 | 9.7 GB | 1.7 s / 4.3 s | 82 tokens/s |
| Embeddings | Qwen3-Embedding-0.6B, Q8_0 | 1.9 GB | 0.75 s / 1.2 s | — |

The two chat models do not fit together next to what Windows already holds,
so models have to be swapped in and out on demand. The GPU does not refuse an
oversized load: Windows evicts other applications' memory instead, so the
budget has to be enforced by the software. Measurements and findings are in
[docs/models.md](docs/models.md).

Some numbers:
- **Extraction** runs on Qwen3.5-4B through the gateway. It takes 0.35–0.6 s
  per message, and got 8 of 8 test sentences and 6 of 6 corrections right
  (relative dates, missing ratings, inferred ingredients). The 9B model did
  no better on the same 12 sentences, at twice the VRAM.
- **Retry budget**: against an upstream that fails every request, 50
  requests at once reach it as 63 attempts instead of 150.
- **Shutdown**: Ctrl+C in the middle of a stream lets the stream finish and
  exits 0; when the grace period runs out, the request is cut off at that
  moment.

## Running it

Requires Linux or WSL2 with Go 1.27+. To try the gateway without a GPU,
against two fake upstreams:

```sh
go run ./cmd/fakeupstream -addr 127.0.0.1:9001 &
go run ./cmd/fakeupstream -addr 127.0.0.1:9002 &
SOUS_APP_KEY=dev-key go run ./cmd/gateway -config config/gateway.fake.yaml &

curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer dev-key' \
  -d '{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

The fake's flags (`-delay`, `-fail-first`, `-break-after` and others) make it
slow or broken in a chosen way, and `POST /_config` changes them while it
runs.

With real models, `config/gateway.yaml` points at three `llama-server`
processes. The bot reads its settings from the environment; `.env.example`
lists them.

## Development

Also requires Python 3.12+ and [uv](https://docs.astral.sh/uv/).

```sh
make check   # gofmt, go vet, go test -race, ruff format/lint, pytest
make fmt     # apply the formatters
```
