# Code signing (free, via SignPath Foundation)

The Vortelio Windows binary (`vortelio-windows-amd64.exe`) is **unsigned**, so
Windows **Smart App Control / SmartScreen** can block it on some machines
(`WinError 4551 — an application control policy has blocked the file`).

The fix is an **Authenticode signature** from a CA that Windows already trusts.
[**SignPath Foundation**](https://signpath.io/open-source) provides this **for
free to open-source projects**. This document is the checklist to get it.

The release workflow (`.github/workflows/release.yml`) already has the signing
step wired in. It only runs when SignPath is configured (see step 4), so the
pipeline keeps shipping an unsigned binary until then — nothing breaks meanwhile.

---

## 1. Eligibility (SignPath Foundation, free OSS tier)

SignPath signs OSS projects for free when, broadly:

- The project is **open source** with an OSI-approved license. ✅ (Vortelio is public)
- The source is in a **public repository** (GitHub). ✅
- Builds run in a **transparent, automated CI** they can inspect (GitHub Actions). ✅
- The project has some real usage / is non-trivial. ✅
- You sign on behalf of the project, not to redistribute someone else's code.

Full, current terms: <https://about.signpath.io/product/open-source>

## 2. Apply

1. Go to <https://signpath.io/open-source> and **request the Foundation (free) plan**.
2. Provide: the GitHub repo URL (`https://github.com/metiutek/Vortelio`), the
   license, a short project description, and the maintainer identity.
3. They review (manual). On approval you get a SignPath **Organization**.

## 3. Configure the SignPath project (after approval)

In the SignPath web console:

1. Create a **Project** (e.g. slug `vortelio`).
2. Add a **GitHub Actions trusted build** pointing at this repo/workflow.
3. Add an **Artifact configuration** for the Windows exe (e.g. slug
   `windows-exe`) — a single Authenticode-signable PE file.
4. Add a **Signing policy** (e.g. slug `release-signing`) bound to the
   Foundation certificate.

Note the four slugs/ids — they go into GitHub below.

## 4. Configure GitHub (repo → Settings)

**Secrets** (Settings → Secrets and variables → Actions → *Secrets*):

| Secret | Value |
|--------|-------|
| `SIGNPATH_API_TOKEN` | the SignPath CI user API token |

**Variables** (same page → *Variables*):

| Variable | Example |
|----------|---------|
| `SIGNPATH_ORG_ID` | your SignPath organization id (GUID) |
| `SIGNPATH_PROJECT_SLUG` | `vortelio` |
| `SIGNPATH_POLICY_SLUG` | `release-signing` |
| `SIGNPATH_ARTIFACT_SLUG` | `windows-exe` |

The `sign-windows` job is gated on `vars.SIGNPATH_ORG_ID != ''`, so it stays
skipped (and the binary stays unsigned) until all of the above exist.

## 5. What happens then

On every `git tag vX.Y.Z` push:

1. `build` compiles all platforms.
2. `sign-windows` submits the Windows exe to SignPath → gets it back **signed**.
3. `release` and `pypi` **prefer the signed Windows binary**, so:
   - the GitHub Release download is signed,
   - the PyPI wheel bundles the signed binary.

> ⚠️ Verify the exact inputs of `signpath/github-action-submit-signing-request`
> against its current README — SignPath versions the action and occasionally
> renames inputs (e.g. how the GitHub artifact id is passed).

## 6. Coverage caveat (git-`main` installs)

Today `uv tool install git+…@main` bundles the binary **committed in the repo**,
which is built locally and **unsigned**. To make *that* path signed too, after a
signed release either:

- commit the signed `vortelio-windows-amd64.exe` back into
  `vortelio-pip/src/vortelio_cli/bin/`, **or**
- switch the launcher to download the signed binary from the GitHub Release
  instead of bundling it (`vortelio-pip/src/vortelio_cli/__main__.py`).

Release-download users and PyPI users get the signed binary immediately.

---

## End-user workaround — running on a PC with Smart App Control ON

If a user hits `WinError 4551 — an application control policy has blocked the
file`, their Windows 11 has **Smart App Control** enabled and it blocks the
unsigned binary. There is **no per-app allow** and **no temporary disable** for
Smart App Control — it's all-or-nothing and, once turned off, **cannot be turned
back on without reinstalling Windows**. The only ways to run the unsigned binary
are below; the user must do this **consciously, on their own machine**.

**Option A — Settings (recommended, reversible only by OS reinstall):**
1. Start → search **Windows Security** → open it
2. **App & browser control**
3. **Smart App Control settings**
4. Set it to **Off** → confirm
5. Re-run `vortelio`

**Option B — PowerShell (same effect as Option A, run as Administrator):**

```powershell
# ⚠️ Requires Administrator. PERMANENTLY disables Smart App Control
# (0 = off, 1 = enforce, 2 = evaluation). A reboot is required.
# Re-enabling later needs a Windows reinstall. Same one-way switch as the UI.
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy" `
  -Name "VerifiedAndReputablePolicyState" -Value 0 -Type DWord
# then reboot the PC
```

> 🚫 **Vortelio must NEVER run this automatically.** Software that disables a
> Windows security feature is exactly what malware does and would get Vortelio
> flagged as a PUA/virus. This is documented only as a manual, informed choice
> the end user makes on their own PC. The clean fix is to **sign the binary**
> (above), so users never have to touch Smart App Control at all.

---

### Alternatives if SignPath OSS is declined

- **Azure Trusted Signing** — ~$10/month, Microsoft-trusted, needs identity
  verification. Works with Smart App Control.
- **Certum Open Source Code Signing** — ~€30/year OV cert for OSS.
- **Self-signed** — free but **useless for distribution** (Windows doesn't trust
  it; still blocked).
