# sous

An OpenAI-compatible LLM gateway (Go), a single-GPU local inference service
(Python), and a cooking-log Discord bot that runs on top of them.

## Development

Requires Linux or WSL2 with Go 1.27+, Python 3.12+ and [uv](https://docs.astral.sh/uv/).

```sh
make check   # gofmt, go vet, go test -race, ruff format/lint, pytest
make fmt     # apply the formatters
```
