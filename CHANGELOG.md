# Changelog

All notable changes to this project. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning: [SemVer](https://semver.org).

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
