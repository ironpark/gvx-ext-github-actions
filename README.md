# GVX Desktop Extension: GitHub Actions (`gvx-ext-github-actions`)

Run GitHub Actions workflows locally with [`nektos/act`](https://github.com/nektos/act) and host isolated ephemeral GitHub Self-Hosted Runners inside GVX (`rungvx`) sandboxes — directly inside GVX Desktop's native declarative UI.

## Features

- **Tab 1 — Local Workflows (`nektos/act`)**
  - Register approved host Git repositories via GVX Desktop's gesture-gated folder picker (`ui:pick-folder`).
  - Automatic discovery of `.github/workflows/*.yml` and `*.yaml` files, trigger events, jobs, `needs` dependencies, and `workflow_dispatch` inputs.
  - Project secrets stored strictly in GVX Desktop's OS Keychain Vault (`/v1/secrets`) with automatic `***` log redaction.
  - Real-time Job/Step DAG view, live streaming masked logs, and one-click **Open Shell** (`ui:terminal`) into active job containers.
  - Built-in scoped Docker API compatibility shim that safely pre-creates extension-owned named volumes (`act-toolcache`, job env/workspace volumes) through `GVX_DOCKER_HOST`.
- **Tab 2 — Self-Hosted Runners (Engine A: Official `actions/runner`)**
  - Runs the official `actions/runner` image as **Standby Ephemeral GVX Sandboxes** via Host `/v1/sandboxes` API.
  - Works behind NAT via outbound GitHub long-polling with one-time JIT configs generated from a Fine-grained GitHub PAT (stored in Host Vault) or manual JIT config tokens.
  - Automatically destroys contaminated sandboxes after each job and replenishes standby capacity up to configured concurrency.
  - Opt-in per-runner Docker-in-Job (`allow_docker`) via scoped `--unix-forward` socket mounting.
  - Automatic Apple Silicon (`arm64`) label selection (`self-hosted, linux, arm64, gvx`) and architecture guidance.
- **Tab 3 — Unified Run History & Synergy**
  - Combined timeline of Local `act` runs and Self-Hosted Runner executions with secret-masked log viewer.
  - One-click **Reproduce Locally with act** loads a runner job into Tab 1 for immediate local debugging.

## Development & Installation

### Build binary (for GVX Desktop Developer Mode)

```bash
go build -o bin/gvx-ext-github-actions ./cmd/gvx-ext-github-actions
```

### Run tests

```bash
go test -race -count=1 ./...
```

### Install in GVX Desktop

1. Enable **Extensions** under **Labs** in GVX Desktop.
2. Open **Extensions** in the sidebar and click **Install from Git**.
3. Enter `https://github.com/ironpark/gvx-ext-github-actions`, review the requested permissions, and click **Allow and build**.
