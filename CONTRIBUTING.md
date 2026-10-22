# Contributing to Vortelio

Thanks for considering a contribution. This doc explains how to set up, what to work on, and how to get a PR merged.

## Quick links
- Bug? → [open an issue](https://github.com/metiu1/Vortelio/issues/new?template=bug_report.yml)
- Feature idea? → [start a discussion](https://github.com/metiu1/Vortelio/discussions) or open a feature request
- Security? → see [SECURITY.md](SECURITY.md), do NOT open a public issue

## Dev setup

```bash
git clone https://github.com/metiu1/Vortelio
cd Vortelio/vortelio
go build ./...
go test ./...
go run ./cmd/vortelio gui
```

Python wrapper:

```bash
cd vortelio-pip
uv build
uv tool install --reinstall ./dist/vortelio-*.whl
```

## PR checklist
- [ ] One logical change per PR
- [ ] `go vet ./...` clean
- [ ] `go test ./...` passes
- [ ] README / CHANGELOG updated if user-visible
- [ ] Version bumped in `vortelio/internal/version/version.go` only for releases
- [ ] Commit messages: Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`)

## Style
- Go: `gofmt`, idiomatic errors, no panics in library code
- JS/HTML in `ui.html`: keep single-file, no build step, no external CDN dependencies for runtime
- Python launcher: stdlib only, support 3.8+

## Releasing (maintainers)
1. Bump the version in `vortelio/internal/version/version.go`, `vortelio-pip/pyproject.toml`, `vortelio-pip/src/vortelio_cli/__init__.py` and `vortelio-pip/src/vortelio_cli/__main__.py`
2. Rebuild the bundled Windows binary into `vortelio-pip/src/vortelio_cli/bin/` (see [BUILD.md](BUILD.md))
3. Update `CHANGELOG.md`
4. Push to `main`: when `version.go` changes, GitHub Actions builds the binaries and publishes the release `vX.Y.Z` automatically. Pushing a tag (`git tag vX.Y.Z && git push --tags`) also publishes the wheel to PyPI

## License
By contributing you agree your work is licensed under [Apache 2.0](LICENSE).
