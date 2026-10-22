# Vortelio (Python wrapper)

Install Vortelio CLI via `uv`:

```bash
uv tool install vortelio
```

From local source:

```bash
uv tool install ./vortelio-pip
```

From a wheel:

```bash
uv tool install ./dist/vortelio-X.Y.Z-py3-none-any.whl
```

Then:

```bash
vortelio gui
vortelio serve --port 11500
vortelio --help
```

## How it works

This package is a thin Python launcher around the Go `vortelio` binary.

1. On install, ships a bundled binary for the host platform when available
   (under `vortelio_cli/bin/`).
2. At runtime, if no bundled binary matches the platform, downloads one from
   `$VORTELIO_RELEASE_BASE` (defaults to the GitHub releases of the matching
   version) into `~/.cache/vortelio/<version>/` and execs it.

## Build / publish

```bash
cd vortelio-pip
uv build              # produces sdist + wheel under dist/
uv publish            # optional, to PyPI
```

To bundle additional platforms, drop binaries named like
`vortelio-linux-amd64`, `vortelio-darwin-arm64`, `vortelio-windows-amd64.exe`
into `src/vortelio_cli/bin/` before `uv build`.
