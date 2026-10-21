# Changelog

All notable changes to this project. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning: [SemVer](https://semver.org).

## [0.3.99] — 2026-10-21
### Changed
- **Download models** lists the same curated, tested image, audio and video models as the studio pages (Z-Image, SD 1.5, FLUX GGUF, Kokoro, DotTTS, Stable Audio, Parakeet, Whisper, Wan…) instead of an old hand-written list of Python models.
- Python models (Bark, CogVideoX, AnimateDiff, diffusers folders, TripoSR 3D) stay available from the command line as an advanced option; the GUI only offers models that run natively.
- CLI help examples use the native models (`image/z-image:turbo`, `audio/kokoro:82m`, `video/wan2.1:1.3b`).
### Removed
- The Python fallback for GGUF video models (it needed a `wan` package that does not exist): an unsupported video GGUF now says which models to install.
- Leftover stable-diffusion.cpp installer code from before the engine manager.

## [0.3.98] — 2026-10-21
### Added
- **Video studio** (sidebar → Video): describe a clip, pick a shape and a length, and generate it on your computer; Wan 2.2 can also start from a picture. Live step progress, results play in place, download, reuse the seed; clips are saved to the Downloads folder. Models install from the page in one click.
- Video runs natively on the same stable-diffusion.cpp server as the images (no Python): **Wan 2.1 1.3B** (recommended, runs on a 4 GB GPU: a 1-second clip takes about 5–10 minutes on a GTX 1650) and **Wan 2.2 5B** text/image-to-video (experimental on 4 GB: very slow). Downloading a model fetches its VAE and the UMT5 text encoder too.
- The engine writes MJPEG AVI: it is converted to H.264 MP4 when ffmpeg is installed, otherwise to an animated GIF.
- API: `GET /api/video/catalog`, `POST /api/video/generate` (server-sent progress, base64 result). The `generate_video` chat tool accepts `seconds`.
### Changed
- `video/wan:1.3b` and `video/wan:14b` (Python diffusers) are replaced by `video/wan2.1:1.3b` and `video/wan2.2:5b`.
### Fixed
- A video generated from the chat without ffmpeg was a GIF saved and labelled as MP4.

## [0.3.97] — 2026-10-20
### Added
- **Image studio** (sidebar → Images): describe a picture, pick a shape (square, portrait, landscape, wide) and generate one or more images; advanced settings for steps, guidance, seed and what to avoid. Results show in a gallery (click to enlarge, download, reuse the seed) and are saved to the Downloads folder. Models install from the page in one click.
- Image models run on a persistent stable-diffusion.cpp server (`sd-server`): the model loads once and stays ready for the next image, with live step progress; it stops after 5 minutes idle or when the chat needs the GPU.
- Model families: each knows the extra files it needs (VAE, CLIP-L, T5-XXL, Qwen3 text encoder) and the settings it was trained for (steps, guidance, sampler, size, negative prompt). Downloading a model fetches its extra files too, shared between models of the same family. This makes FLUX.1 and **Z-Image Turbo** work (new default: best quality, readable text, runs on a 4 GB GPU).
- API: `GET /api/image/catalog`, `POST /api/image/generate` (server-sent progress, base64 PNG results), `POST /api/image/unload`.
### Changed
- The `generate_image` chat tool and `/v1/images/generations` use the native engine with the model's own defaults; the tool accepts `width`, `height` and `negative_prompt`, the OpenAI endpoint honours `size` and generates `n` images in one batch. Without a model name the best installed image model is used.
- `image/openjourney` is now called `image/sd1.5` (the old name still works).
### Fixed
- Every image reloaded the model from disk and used Stable Diffusion 1.5 settings (CFG 7, 512×512, negative prompt) for every model; FLUX models could not run at all because their VAE and text encoders were never downloaded.
- `vortelio run` with an image or audio model no longer leaves the engine running after the command ends.

## [0.3.96] — 2026-10-20
### Added
- **Audio studio** (sidebar → Audio & music): Speak, Clone a voice, Edit speech, Music, Sound effects, Separate tracks and Transcribe, each with its own page. Recordings can be uploaded in any format or recorded from the microphone; results play in place and can be downloaded. Models install from the page in one click (Downloads panel).
- Native audio engines, no Python: audio.cpp for speech (Kokoro: 54 voices including Italian), voice cloning and speech editing (DotTTS-Edit, multilingual; VoxCPM), music and sound effects (Stable Audio 3 Small), stem separation (HT-Demucs: vocals, drums, bass, other) and transcription (Parakeet, 25 languages); whisper.cpp for Whisper models. One audio model is loaded at a time, the GPU is taken from the chat model only while it works, and an idle engine stops after 5 minutes.
- Curated audio catalog, all tested end to end on a 4 GB GPU. Any audio.cpp GGUF works too: its family, tasks, languages and voices are read from the file.
- API: `GET /api/audio/catalog`, `POST /api/audio/{speech,generate,edit,separate,transcribe}`. The chat assistant gains a `generate_music` tool, and `text_to_speech` accepts a voice.
### Changed
- `/v1/audio/speech`, `/v1/audio/transcriptions` and `/v1/audio/translations` run on the native engines and pick a model that can do the task (an OpenAI name such as `tts-1` or `whisper-1` falls back to the best installed one). OpenAI voice names map to the model's voices (`nova` → `af_nova`). `response_format` mp3/ogg/flac/opus/aac is honoured when ffmpeg is installed; otherwise WAV is returned with the right content type (it was labelled mp3).
- `audio/kokoro:latest` now installs the native Kokoro model.
### Fixed
- Model manifests saved with a UTF-8 BOM (e.g. edited in Notepad) made the model disappear.

## [0.3.95] — 2026-10-20
### Changed
- Local models are placed on the GPU by llama.cpp's own memory fitting (`--fit` / `llama-fit-params`), measured against the VRAM that is actually free, instead of a guess that assumed every model has 40 layers. The safety margin scales with the card (~8%, 256–1024 MiB) instead of llama.cpp's fixed 1 GiB, which on a 4 GB GPU cost a third of the layers. Measured on a GTX 1650 with qwen3.5:4b: 5.8 → 11–18 tok/s while another app shares the GPU, up to 28 tok/s with the card free.
- Default context 8192 tokens (was 4096). Native chat templates (`--jinja`) for every model.
- One model resident at a time: loading another model, or a different context size, first stops the previous one (`VORTELIO_MAX_LOADED_MODELS` raises the limit). Image, video, audio and 3D generation free the GPU from the chat model first; a chat that called such a tool reloads its model automatically and continues (`VORTELIO_GPU_SHARE=1` keeps everything resident).
- GGUF downloads fetch every shard of split quants and, for chat models, the vision projector (`mmproj`), which is then used automatically. Without a quant in the reference, Q4_K_M is picked.
- `/api/ps` reports the VRAM a model really uses.
### Fixed
- Think off was ignored by reasoning models (Qwen 3.5, GLM…): they kept thinking and, with a token limit, returned an empty answer. The switch now reaches the model's chat template.
- Embeddings (`/api/embed`, `/api/embeddings`, `/v1/embeddings`, RAG) returned `null` vectors: they now run on a dedicated embedding server with mean pooling.
- A model that once failed on the GPU (for example while another app held the VRAM) stayed on the CPU forever; it is now retried on the GPU.
- Two simultaneous requests for a model no longer start it twice; a crashed llama-server is detected and restarted.
- Long tool-call arguments (e.g. a whole file to write) could cut the response off silently (64 KB line limit).
- Ollama responses now carry real `eval_count`, `eval_duration`, `prompt_eval_count` and `load_duration`.
- GGUF inspection misread every key after a large vocabulary and was very slow.

## [0.3.94] — 2026-10-20
### Added
- Downloads panel (sidebar → Downloads): every model and engine download is a background job. It keeps going when the page is closed, resumes by itself after Vortelio restarts, and can be paused, resumed, retried or cleared. Shows size, speed and time left.
- Native engines (Downloads → Engines): stable-diffusion.cpp (images, video), audio.cpp (speech, music, voice editing and cloning) and whisper.cpp (speech to text) install as pinned prebuilt releases — no Python. Each install is checked against the release SHA-256, must start before it replaces the working one, and the previous version is kept for one-click restore. CUDA builds share one copy of the CUDA runtime (hard links), reusing llama.cpp's when present.
- Hugging Face token for gated models (Downloads → Engines, or `HF_TOKEN`).
- API: `GET /api/downloads`, `GET /api/downloads/events` (SSE), `POST /api/downloads/{start,cancel,retry,dismiss}`, `GET /api/engines`, `POST /api/engines/{install,rollback,remove}`, `GET/POST /api/hf/token`.
### Changed
- Model downloads are resumable: an interrupted transfer continues from where it stopped (HTTP Range) instead of starting over, dropped or stalled connections are retried automatically, and every file is checked against the size and SHA-256 published by Hugging Face before it is used. A truncated file is no longer mistaken for a complete one.
- `/api/pull` no longer cancels the download when the client disconnects; it follows the background job.
- Image generation uses the pinned stable-diffusion.cpp engine instead of the latest upstream build.
### Fixed
- Streaming responses (chat tokens, download progress) were held back in 4 KB chunks: the request logger hid `http.Flusher` from every handler.

## [0.3.93] — 2026-09-29
### Added
- Any OpenAI-compatible (`/v1/chat/completions`: LM Studio, vLLM, LocalAI, LiteLLM, llama.cpp, Ollama, another Vortelio…) or Anthropic-compatible (`/v1/messages`) endpoint can be added as a provider, with or without an API key. Custom providers are saved server-side (`~/.vortelio/cloud_providers.json`) and work everywhere: web UI, `vortelio code`, the menu TUI, the unified `/v1` gateway and agents.
- `vortelio code` `/model`: "＋ Add provider / API key" configures OpenAI, Anthropic, OpenRouter, Gemini, Groq, Mistral, xAI, DeepSeek, Together, Perplexity, Ollama Cloud or a custom endpoint without leaving the terminal (key input is masked). "＋ Browse all models of a provider" lists the provider's live catalog (`/v1/models`) with type-to-filter.
- Web UI: "Add cloud model" loads the provider's live model list; custom endpoints choose the API format (OpenAI / Anthropic).
- API: `POST/DELETE /api/cloud/custom`, `GET /api/cloud/models?provider=`.
### Changed
- `/model` in `vortelio code` now lists the models of every configured provider, including paid ones (OpenAI, Anthropic…), which previously showed nothing.
- Anthropic model list updated (Claude Opus 5.5, Sonnet 5.5, Fable 5.1, Haiku 4.5).

## [0.3.92] — 2026-09-29
### Changed
- Web UI, Agent → Developer: cleaner IDE-style workspace. The working folder is a compact pill in the composer instead of a full-width path bar, and it is remembered between sessions. The file explorer shows the folder name, line icons and expand arrows. Tool calls are one quiet row — status (spinner, check, cross), a readable action ("Read", "Run", "Edit"…) and its target (path, command, query) — expandable for arguments and result.
- Capability chips without emoji; notifications appear at the top instead of covering the composer; the duplicated "Mode: …" label is gone (the Plan/Ask/Auto menu stays).

## [0.3.91] — 2026-09-29
### Changed
- `vortelio code` `/model` lists only models usable on each provider's free plan: Gemini (3.8/3.5 Flash, 2.5 Pro/Flash/Flash-Lite), Groq (gpt-oss 120B/20B, Qwen3.8 27B), Mistral (Large, Medium, Small, Codestral), OpenRouter (free models, checked against the live catalog) and Ollama Cloud. Providers without a free plan list nothing.
- "＋ Custom model…" asks for the provider, then just the model id; custom models are remembered and work with every provider.
- `vortelio code` no longer streams the model's reasoning by default: only the "Thinking…" spinner shows. Ctrl+O or `/thinking on` reveals it; an explicit `show_thinking` setting is still honoured. (Version 0.3.90 was never released on its own; its changes ship in 0.3.91.)

## [0.3.89] — 2026-09-29
### Added
- `vortelio code` `/model`: "＋ Custom model…" entry; custom cloud models are remembered and shown again for providers with a key.
### Changed
- Ollama Cloud picker in `vortelio code` shows a short featured list instead of the full live catalog.

## [0.3.88] — 2026-09-28
### Added
- `vortelio code` rewritten in the style of Claude Code / Codex / OpenCode: permission modes (plan · ask · edits · auto, Shift+Tab), allow/deny rules, colored diffs in approvals, todo list, AGENTS.md/CLAUDE.md project memory with `/init` and `# note`, saved sessions (`/resume`, `-c`, `-r`), `/compact` (automatic when the context fills up), `/undo`, `/diff`, `/context`, `/status`, `/config`, `/permissions`, custom `/commands`, `!shell`, `@file`, Esc to interrupt, multi-line input, and a non-interactive `-p` mode with JSON output.
- Coding tools follow the usual conventions: numbered paged reads, unique exact-match edits (`replace_all`), read-before-edit, regex grep with include filter, real `**` globs, shell timeout and exit code.
- Ollama Cloud models are listed live from ollama.com (cheapest first, with prices); updated offline list.
- Web UI approvals show a colored diff for file edits.
### Fixed
- Reasoning of thinking models (Qwen 3.5, Gemma 4, gpt-oss, DeepSeek…) was never shown: llama.cpp and cloud providers send it in a separate field that was dropped. It now streams in the web UI, `vortelio run` and `vortelio code`.
- Local answers were cut at 512 tokens (reasoning models often returned nothing) and streams longer than 2 minutes were aborted.
- Plan and ask modes were bypassed: builtin file tools shadowed the approval-gated coding tools.
- `vortelio code` sent no system prompt to local models, used a 4K context and truncated every tool result to 2,000 characters.
- `--flash-attn` was passed to llama-server without its value.

## [0.3.87] — 2026-09-28
### Fixed
- Windows: updating from the web GUI or `vortelio update` did nothing. The updater was killed together with uv's launcher job when Vortelio exited; it now starts outside that job and completes.
### Added
- Auto-update (off by default): Settings → Updates → Auto-update, or `vortelio update --auto on|off`. The server installs new versions when idle and restarts itself.

## [0.3.86] — 2026-09-28
### Changed
- Model catalog refreshed: Qwen 3.5 (0.8B/4B/9B), Qwen 3.8 27B, Qwen 3.6 35B-A3B, Qwen3 Coder 30B, Gemma 4 (E2B/E4B/12B/26B), gpt-oss 20B. Older models stay installable by name.
- Onboarding recommends `qwen3.5:4b`; Gemma 4 recognised as tool-capable.

## [0.3.85] — 2026-09-28
### Changed
- New UI: calmer, minimal look — SVG icons, indigo accent, fewer controls on screen, simpler model catalog.
### Added
- llama.cpp is installed automatically on first use and kept up to date (at startup, and when a model needs a newer engine). Settings shows the engine build with a one-click update.
### Fixed
- Chat stayed silent when llama.cpp was missing.
- llama.cpp download on macOS/Linux (upstream switched to `.tar.gz`, shared-library symlinks).

## [0.3.84] — 2026-09-28
### Fixed
- Linux/macOS builds failed (`shellQuote` undefined), so no release was published after v0.3.76 and `vortelio` returned HTTP 404 on first run. Releases are now published automatically when the version changes.

## [Unreleased]
### Added
- Python wrapper package (`vortelio-pip/`) — install via `uv tool install vortelio` or `pip install vortelio`
- Cross-platform launcher: bundles binary in wheel, downloads from GitHub Releases on miss
- GitHub Actions CI + release workflow
- Issue / PR templates, CONTRIBUTING, CODE_OF_CONDUCT, SECURITY

## [0.3.49] — 2026-04-11
### Fixed
- Auth overlay invisible: missing `position:fixed` CSS — Account button appeared dead
- `authShowOverlay()` resets form, focuses email
- `authApplySession()` shows overlay correctly after logout

## [0.3.47] — 2026-04-10
### Fixed
- Code on single line: AI text nodes switched from `<span>` to `<div>` to preserve `white-space:pre`
- Account dropdown menu HTML was never inserted in DOM
### Added
- Full README with file structure, build instructions, API routes, data paths

## [0.3.46] — 2026-04-10
### Added
- Account dropdown: Settings, Change password, Sign out
### Fixed
- Kokoro TTS on Python 3.14: `kokoro-onnx` primary backend (no `espeak-ng`), `kokoro` fallback for ≤3.12
- Code block CSS: dark bg, full width, copy button top-right

## [0.3.45] — 2026-04-09
### Fixed
- Topbar layout: Settings/Save left, Account right — no overlap
- Ollama Cloud latency: direct fetch to `ollama.com`, no Go proxy buffering
- Audio SyntaxError: `buildWhisperScript` rewritten as string list

## [0.3.44] — 2026-04-09
### Fixed
- GUI disconnected: `init()` called before `#auth-overlay` in DOM → silent crash → polling never started. Wrapped in `DOMContentLoaded`
- Hardware detection cached with `sync.Once` (was running `nvidia-smi` per `/api/status`)
- `service_windows.go` looked for `pullai-server.exe` instead of `vortelio-server.exe`

## [0.3.43] — 2026-04-09
### Fixed
- GUI red dot: `/api/status` timeout raised to 8s, retry every 2s

## [0.3.42] — 2026-04-08
### Fixed
- `DetectHardware()` cached, warmed up in background at server start
- Edge/Chrome profile dir moved to `~/.vortelio/app-profile`

## [0.3.41] — 2026-04-07
### Fixed
- Server exe lookup: `vortelio-server.exe` → `pullai-server.exe` → self (back-compat)
- Removed last `pullai` strings from CLI error messages

## [0.3.40] — 2026-04-06
### Fixed
- Ollama Cloud 401: endpoint fixed `/api/chat` → `/v1/chat/completions`
- Cloud model list cleanup, removed non-existent `qwen3-vl-235b-cloud`
- IT↔EN translations completed for cloud / agents / auth panels
- GUI polling every 2s instead of waiting 15s

## [0.3.39] — 2026-04-05
### Fixed
- Audio on Python 3.14: `faster-whisper` primary backend (no ctypes crash)
- Installer: `/oname=vortelio.exe` rename works
### Added
- Gemini API: `streamGenerateContent?alt=sse`, header `x-goog-api-key`

## [0.3.38] — 2026-03-30
### Added
- Rename Votelio → Vortelio across codebase
- Cloud API panel: 8 providers (Ollama, OpenAI, Anthropic, Gemini, Groq, Mistral, OpenRouter)
- AI agents: OpenClaw, Open Code with install/start/stop/web UI
- Auth: email+password (SHA-256+salt), Google OAuth, session in localStorage
- Code block rendering during streaming with dark theme + copy button
### Fixed
- NSIS installer: `/oname=` to rename exe during install

[Unreleased]: https://github.com/metiu1/Vortelio/compare/v0.3.49...HEAD
[0.3.49]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.49
[0.3.47]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.47
[0.3.46]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.46
[0.3.45]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.45
[0.3.44]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.44
[0.3.43]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.43
[0.3.42]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.42
[0.3.41]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.41
[0.3.40]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.40
[0.3.39]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.39
[0.3.38]: https://github.com/metiu1/Vortelio/releases/tag/v0.3.38
