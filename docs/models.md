# Models

The local models behind the `extract`, `chat` and `embed` logical names, the
engine that serves them, and what each one costs on the development machine.
All numbers were measured on 2026-11-12.

## Machine

- GPU: RTX 5080, 16 GB (16303 MiB), compute capability 12.0. Windows driver
  616.64; WSL2 sees CUDA 13.4.
- WSL2 Ubuntu 24.04 with 15 GB of RAM and 20 threads.
- Windows itself holds 1–2.5 GB of VRAM when idle (desktop, browser), so about
  13.5 GB is reliably free.

## Engine

llama.cpp's `llama-server`, build b12336 (commit `cfb9d4b75`). It speaks the
OpenAI API, serves one model per process, and answers `/health` with 503 while
loading and 200 once ready.

Installed from the official prebuilt `ubuntu-cuda-12.8-x64` archive plus its
matching `cudart` archive, both extracted into `~/opt/llama.cpp-b12336/`. The
CUDA 12.8 build was chosen over 13.5 because the driver only supports CUDA 13.4.

```
$ ~/opt/llama.cpp-b12336/llama-server --list-devices
Available devices:
  CUDA0: NVIDIA GeForce RTX 5080 (16275 MiB, 14953 MiB free)
```

## Models

| Logical name | Model | File | Size | Source |
|---|---|---|---|---|
| `extract` | Qwen3.5-4B | `Qwen3.5-4B-Q8_0.gguf` | 4.5 GB | `unsloth/Qwen3.5-4B-GGUF` |
| `chat` | Qwen3.5-9B | `Qwen3.5-9B-Q8_0.gguf` | 9.5 GB | `unsloth/Qwen3.5-9B-GGUF` |
| `embed` | Qwen3-Embedding-0.6B | `Qwen3-Embedding-0.6B-Q8_0.gguf` | 0.64 GB | `Qwen/Qwen3-Embedding-0.6B-GGUF` |

The files live in `~/models/` on the WSL disk, not under `/mnt/c`: reading
gigabytes through the Windows filesystem would stretch every load. They are
not in git. Each was checked against the SHA-256 that Hugging Face lists for it.

## Measurements

| Model | Flags | VRAM | Load (cold) | Load (warm) | Generation |
|---|---|---|---|---|---|
| `extract` | `-ngl all -c 8192` | 5.1 GB | 2.6 s | 1.8 s | 134 tokens/s |
| `chat` | `-ngl all -c 32768` | 9.7 GB | 4.3 s | 1.7 s | 82 tokens/s |
| `embed` | `-ngl all -c 8192 --embedding --pooling last` | 1.9 GB | 1.2 s | 0.75 s | 4 short inputs in 46 ms |

- **VRAM**: the rise in `nvidia-smi` used memory from before start to ready.
  Requests afterwards added at most 70 MiB.
- **Load**: from process start until `/health` returns 200. Cold is right
  after dropping the Linux page cache; warm is with the file still cached.
- **Streaming**: for a 21-token prompt, the first content chunk arrived after
  78 ms (`extract`) and 73 ms (`chat`), then one chunk every 7–12 ms.

## Findings

1. **Both chat models think by default.** With the default chat template, a
   request with `max_tokens: 1024` spent all 1024 tokens in
   `reasoning_content` and returned empty `content` (7.8 s on the 4B, 12.7 s on
   the 9B). With `enable_thinking` off, the same question took 0.63 s and
   0.85 s. The server flag
   `--chat-template-kwargs '{"enable_thinking":false}'` turns it off, and a
   request can still turn it back on with
   `"chat_template_kwargs": {"enable_thinking": true}`. `extract` must run with
   thinking off.
2. **Always set `-c`.** The default context size is the model's maximum,
   262,144 tokens for Qwen3.5. By default the server runs 4 parallel slots that
   share one KV cache of size `-c`.
3. **Always set `-ngl` and `-c` explicitly.** `--fit` is on by default and
   adjusts any unset value to whatever VRAM is free at that moment. A model's
   footprint should not depend on what else happens to be running.
4. **Every server process costs about 0.6 GB on top of its weights**, for the
   CUDA context and compute buffers. Even with `-c 2048 -np 1`, the 0.64 GB
   embedding model takes 1.2 GB.
5. **`-ub 8192`, as given in the Qwen3-Embedding README, does nothing by
   itself.** The micro-batch cannot exceed the batch size, which defaults to
   2048: VRAM was the same as with `-ub 2048`.
6. **Free VRAM is not a hard wall.** Loading `extract` and `chat` together
   (14.8 GB, plus what Windows held) succeeded. Windows moved other
   applications' VRAM out to make room: once both servers stopped, usage fell
   to 0.9 GB. Both models still generated at full speed. A memory budget
   therefore has to be enforced by the service itself.
7. **Per-process GPU memory is not visible from WSL.** `nvidia-smi` lists the
   `llama-server` process but reports its memory as N/A. Only the total is
   available.
8. **The embeddings look sane.** The vectors are 1024-dimensional and already
   normalised. cos(鱼香茄子, 红烧茄子) = 0.771, cos(鱼香茄子, 番茄炒蛋) =
   0.535, cos(鱼香茄子, 红烧肉) = 0.515. Server flags moved these in the third
   decimal (0.769 with `-np 1`), so keep the embedding server's flags fixed.

## Running a model by hand

```bash
# extract, thinking off
~/opt/llama.cpp-b12336/llama-server -m ~/models/Qwen3.5-4B-Q8_0.gguf \
  --port 8081 -ngl all -c 8192 --chat-template-kwargs '{"enable_thinking":false}'

# chat
~/opt/llama.cpp-b12336/llama-server -m ~/models/Qwen3.5-9B-Q8_0.gguf \
  --port 8082 -ngl all -c 32768

# embed
~/opt/llama.cpp-b12336/llama-server -m ~/models/Qwen3-Embedding-0.6B-Q8_0.gguf \
  --port 8083 -ngl all -c 8192 --embedding --pooling last
```

```bash
curl -N localhost:8081/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"用两三句话介绍一下鱼香茄子怎么做。"}],"stream":true}'

curl localhost:8083/v1/embeddings -H 'Content-Type: application/json' \
  -d '{"input":["鱼香茄子","红烧茄子"]}'
```

## Extraction trial

Five made-up notes, each aimed at one difficulty. Each was sent with a system
prompt that defines the five fields and gives today's date (2026-11-12, a
Thursday), with `temperature: 0`, thinking off, and `response_format` set to a
JSON schema. llama-server turns the schema into a grammar, so every reply was
valid JSON.

| # | Note | Difficulty |
|---|---|---|
| 1 | 今天做了鱼香茄子，少放了糖，9分 | no ingredients given |
| 2 | 昨晚番茄炒蛋，西红柿买的太生了有点酸，7分吧，下次放点糖 | relative date, 西红柿 = 番茄, "7分吧" |
| 3 | 周末炖了个土豆牛腩，牛腩500g，土豆两个，胡萝卜一根，炖了一个半小时，软烂，满分！ | "周末", quantities, "满分" = 10 |
| 4 | 麻婆豆腐和蒜蓉西兰花，豆腐有点碎了，西兰花不错 | two dishes, no rating |
| 5 | 可乐鸡翅糊锅了😭 三分 下次火小点 | rating in Chinese numerals, emoji, no date |

Results, judged against the prompt's rules:

| # | Qwen3.5-4B | Qwen3.5-9B |
|---|---|---|
| 1 | Name, rating and date right. Twelve ingredients, seasonings included. The notes add "下次需要增加糖的用量", which the note never said. | Right, but ten ingredients, seasonings included. |
| 2 | Right; keeps 西红柿. Aliases are the app's job. | Right. |
| 3 | Right, except 周末 became Sunday 11-08 instead of Saturday. | Right. |
| 4 | Two records, but **invented ratings of 6 and 9**, and embellished notes. | Right: ratings null, notes as written. |
| 5 | Right. | Right. |

Latency was 0.8–1.7 s for the 4B and 1.1–1.8 s for the 9B. With outputs this
short, prompt processing dominates, so the larger model costs little here.

- A schema fixes the format, not the content: the 4B filled fields it should
  have left empty. Model output is untrusted, and the confirmation message has
  to show every field.
- Both models list seasonings as ingredients; the prompt needs a rule for that.
- Five notes are too few to choose between the models for `extract`. The
  extraction eval will decide.
