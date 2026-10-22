# How to build Vortelio-Setup-X.Y.Z.exe

Complete guide to produce the Windows installer from source.

---

## Prerequisites

| Tool | Minimum version | Download |
|------|-----------------|----------|
| **Go** | 1.22+ | https://go.dev/dl/ |
| **NSIS** | 3.09+ | https://nsis.sourceforge.io/Download |
| **Git** | any | https://git-scm.com |

On Windows with Chocolatey (administrator PowerShell):
```powershell
choco install golang nsis git
```

NSIS is installed to `C:\Program Files (x86)\NSIS\`.

---

## Relevant structure

```
Vortelio/
├── vortelio/                        # Go source
│   ├── cmd/vortelio/main.go
│   ├── go.mod
│   ├── internal/version/version.go  # ← version string
│   └── dist/                        # binary output folder (created by build)
├── vortelio-installer/
│   ├── pullai-installer.nsi          # NSIS script
│   ├── build/
│   │   ├── pullai.exe               # ← main binary (copied here)
│   │   └── pullai-server.exe        # ← same copy (background server)
│   └── assets/
│       └── pullai.ico
└── BUILD.md                         # this file
```

---

## Update the version (optional)

Before building, if you want to change the version update **all** of these files with the same number:

**`vortelio/internal/version/version.go`**
```go
var Version = "X.Y.Z"   // ← change here
```

**`vortelio-pip/pyproject.toml`**, **`vortelio-pip/src/vortelio_cli/__init__.py`**, **`vortelio-pip/src/vortelio_cli/__main__.py`**
(the Python launcher caches the binary per version, so a missed bump keeps running the old one)

**`vortelio-installer/pullai-installer.nsi`** (line ~16)
```nsi
!define PRODUCT_VERSION    "X.Y.Z"   ; ← same number
```

---

## Step-by-step build

### 1. Compile the Go binary

```bash
cd vortelio
go build -o dist/vortelio-windows-amd64.exe ./cmd/vortelio/
```

Output: `vortelio/dist/vortelio-windows-amd64.exe`

### 2. Copy the binaries to the installer build folder

```bash
cp vortelio/dist/vortelio-windows-amd64.exe vortelio-installer/build/pullai.exe
cp vortelio/dist/vortelio-windows-amd64.exe vortelio-installer/build/pullai-server.exe
```

> NSIS renames `pullai.exe` → `vortelio.exe` and `pullai-server.exe` → `vortelio-server.exe`
> during installation via the `/oname=` directive in the .nsi script.

### 3. Create the installer with NSIS

```bash
"C:\Program Files (x86)\NSIS\makensis.exe" vortelio-installer/pullai-installer.nsi
```

Output: `vortelio-installer/Vortelio-Setup-X.Y.Z.exe`

---

## Quick build (single copy-paste block)

Run everything from the repository root folder:

```bash
cd vortelio && \
go build -o dist/vortelio-windows-amd64.exe ./cmd/vortelio/ && \
cp dist/vortelio-windows-amd64.exe ../vortelio-installer/build/pullai.exe && \
cp dist/vortelio-windows-amd64.exe ../vortelio-installer/build/pullai-server.exe && \
cd ../vortelio-installer && \
"C:/Program Files (x86)/NSIS/makensis.exe" pullai-installer.nsi
```

---

## Expected output

```
Output: "...\vortelio-installer\Vortelio-Setup-X.Y.Z.exe"
...
Total size: ~5.5 MB / ~11 MB (49%)
```

1 harmless warning (`unknown variable "_"`) is normal and does not block the build.

---

## Troubleshooting

| Problem | Cause | Solution |
|---------|-------|----------|
| `makensis: command not found` | NSIS not in PATH | Use the full path `"C:\Program Files (x86)\NSIS\makensis.exe"` |
| `go: module not found` | Missing dependencies | Run `go mod tidy` in the `vortelio/` folder |
| `build/pullai.exe not found` | Step 2 skipped | Repeat the binary copy before running NSIS |
| Installer does not install the updated version | Version not updated in .nsi | Update `PRODUCT_VERSION` in `pullai-installer.nsi` |
