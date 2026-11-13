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

## Development

Requires Linux or WSL2 with Go 1.27+, Python 3.12+ and [uv](https://docs.astral.sh/uv/).

```sh
make check   # gofmt, go vet, go test -race, ruff format/lint, pytest
make fmt     # apply the formatters
```
