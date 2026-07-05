"""Launcher: locate bundled vortelio binary, exec with passed args."""
from __future__ import annotations

import os
import platform
import shutil
import stat
import subprocess
import sys
import urllib.request
from pathlib import Path

VERSION = "0.3.83"
RELEASE_BASE = os.environ.get(
    "VORTELIO_RELEASE_BASE",
    f"https://github.com/metiu1/Vortelio/releases/download/v{VERSION}",
)

BIN_DIR = Path(__file__).parent / "bin"


def _binary_name() -> str:
    sysname = platform.system().lower()
    arch = platform.machine().lower()
    arch = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(arch, arch)
    if sysname.startswith("win"):
        return f"vortelio-windows-{arch}.exe"
    if sysname == "darwin":
        return f"vortelio-darwin-{arch}"
    return f"vortelio-linux-{arch}"


def _cache_dir() -> Path:
    base = os.environ.get("XDG_CACHE_HOME") or (Path.home() / ".cache")
    d = Path(base) / "vortelio" / VERSION
    d.mkdir(parents=True, exist_ok=True)
    return d


def _download(url: str, dest: Path) -> None:
    tmp = dest.with_suffix(dest.suffix + ".part")
    print(f"vortelio: downloading {url}", file=sys.stderr)
    with urllib.request.urlopen(url) as r, open(tmp, "wb") as f:
        shutil.copyfileobj(r, f)
    tmp.replace(dest)
    if os.name != "nt":
        dest.chmod(dest.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def _resolve_binary() -> Path:
    name = _binary_name()
    cached = _cache_dir() / name
    bundled = BIN_DIR / name
    # IMPORTANT: always run the binary from the cache dir, never directly from the
    # package inside the uv venv. A running server keeps the exe open; if that exe
    # lived in the venv, the next `uv tool install/uninstall` could not replace it
    # ("Access denied") and would leave a half-removed install
    # (ModuleNotFoundError: vortelio_cli). The cache is version-scoped and outside
    # the venv, so the venv is never locked.
    if not cached.exists():
        if bundled.exists():
            try:
                cached.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(bundled, cached)
                if os.name == "nt":
                    # Strip the "Mark of the Web" so Windows SmartScreen doesn't
                    # block the copied binary as "downloaded from the internet".
                    try:
                        os.remove(str(cached) + ":Zone.Identifier")
                    except OSError:
                        pass
                else:
                    cached.chmod(cached.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
            except Exception:
                return bundled  # fall back to in-package binary
        else:
            url = f"{RELEASE_BASE}/{name}"
            try:
                _download(url, cached)
            except Exception as e:
                sys.stderr.write(
                    f"vortelio: no bundled binary for this platform ({name}) and "
                    f"download failed: {e}\nSet VORTELIO_RELEASE_BASE or place a binary at {bundled}\n"
                )
                sys.exit(1)
    return cached


def _prune_old_cache() -> None:
    """Remove cached binaries from previous versions so updates don't pile up
    38MB folders forever. Best-effort: a still-running old server keeps its exe
    locked, so that folder is skipped and cleaned on a later run."""
    try:
        base = _cache_dir().parent  # ~/.cache/vortelio
        for child in base.iterdir():
            if child.is_dir() and child.name != VERSION:
                shutil.rmtree(child, ignore_errors=True)
    except Exception:
        pass


def _blocked_by_policy(err: OSError) -> bool:
    if getattr(err, "winerror", None) == 4551:
        return True
    msg = str(err).lower()
    return "criterio di controllo" in msg or "application control" in msg or "blocked" in msg


def _explain_policy_block(binary: Path) -> None:
    sys.stderr.write(
        "\n"
        "⚠ Windows ha bloccato l'avvio di Vortelio (Smart App Control / criterio di\n"
        "controllo applicazioni). Il binario non è ancora firmato digitalmente.\n\n"
        "Per consentirlo (una volta sola):\n"
        "  Impostazioni → Privacy e sicurezza → Sicurezza di Windows →\n"
        "  Controllo app e browser → Smart App Control → impostalo su 'Disattivato'.\n"
        "  (Su PC aziendali può essere una policy AppLocker/WDAC: chiedi all'IT\n"
        "   di consentire questo eseguibile.)\n\n"
        f"Eseguibile bloccato:\n  {binary}\n\n"
        "In alternativa, scarica e avvia l'exe dalle Release:\n"
        "  https://github.com/metiu1/Vortelio/releases/latest\n"
    )


def main() -> int:
    binary = _resolve_binary()
    _prune_old_cache()
    args = sys.argv[1:]
    try:
        if os.name == "nt":
            # `gui` just starts a detached background server and opens a window,
            # then it's done. Launch it fully detached and exit immediately, so
            # this python process (which lives in the uv venv) doesn't linger and
            # lock the venv — a lingering launcher is what made `uv tool install`
            # fail with "Access denied" and corrupt the install.
            if args[:1] == ["gui"]:
                DETACHED_PROCESS = 0x00000008
                CREATE_NEW_PROCESS_GROUP = 0x00000200
                subprocess.Popen(
                    [str(binary), *args],
                    creationflags=DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP,
                    close_fds=True,
                )
                return 0
            return subprocess.call([str(binary), *args])
        os.execv(str(binary), [str(binary), *args])
        return 0  # unreachable
    except OSError as e:
        if _blocked_by_policy(e):
            _explain_policy_block(binary)
            return 1
        raise


if __name__ == "__main__":
    sys.exit(main())
