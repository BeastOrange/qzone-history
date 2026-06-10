# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Overview

QZone History Restoration reconstructs a user's QQ-space (QQ空间) history. It authenticates
via QR code, scrapes the activity feed (the only place where interactions with **deleted**
posts still appear), and rebuilds `Moment`s and `BoardMessage`s by inference from those
activities. **All user-facing strings and most comments are in Chinese** — match that when
editing.

> The repo states it is for studying clean architecture only.

## Two independent programs

This repo contains **two separate Go modules** that do not share code:

1. **Main app** — module `qzone-history` (root `go.mod`, Go 1.25.2). Clean-architecture
   reconstruction tool that persists to SQLite via GORM. Entry: `cmd/main.go`.
2. **`oneclick/`** — module `qzone` (`oneclick/go.mod`, Go 1.26.4). A **zero-dependency,
   stdlib-only single-file** tool (`oneclick/main.go`) for end users: scan QR → fetch all
   *existing* moments via the official `emotion_cgi_msglist_v6` API → download original-size
   images (multi-threaded, with URL fallbacks) → base64-embed them into one self-contained,
   searchable HTML file with a click-to-zoom lightbox. Since v1.1 it **also** reconstructs
   *suspected-deleted* moments by separately scanning the activity feed
   (`feeds2_html_pav_all`) and inferring posts from like/comment/view traces — a stdlib-regex
   re-implementation of the main app's reconstruction idea, but it shares **no code** with the
   main app. Marks reconstructed posts with 🕯 and writes a detailed run log
   (`QzoneExport_log_<time>.txt`) next to the binary. Has Python equivalents
   (`qzone_export.py`, `patch_lightbox.py`, `run.bat`).

When changing one, do not assume the other needs the same change — they are unrelated. The
two CLAUDE.md / AGENTS.md files are kept as mirror copies (Claude vs. Codex); update both when
editing repo guidance.

## Commands

```bash
# Main app (from repo root)
go build -o qzone-history ./cmd/main.go
go run ./cmd/main.go            # runs the full pipeline (see below)
go mod tidy

# oneclick (separate module — must cd in)
cd oneclick && go build -o QzoneExport .
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o QzoneExport.exe .   # cross-compile
go test ./...                  # oneclick is the only module with tests (main_test.go)

# Releases are built by GoReleaser on a v* tag push (.github/workflows/release.yml).
# .goreleaser.yml builds ONLY the main app (./cmd/main.go) with CGO_ENABLED=0; version is
# injected via -ldflags "-X version.Version={{.Version}}". oneclick binaries are NOT built by
# goreleaser — they are cross-compiled by hand and attached to `oneclick-vX.X` releases.
```

Tests live only in `oneclick/main_test.go` (the main app has none); run them with
`cd oneclick && go test ./...`. They cover the regex feed parser, JS-unescape, multi-segment
JSONP handling, reconstruction/dedup, image-candidate fallbacks, and download concurrency —
the fragile parsing paths, so run them after touching `parseActivities`, `processOldHTML`,
`reconstructMoments`, or the image pipeline. The dev environment may pin `QZONE_DOWNLOAD_CONCURRENCY`.

## Main app architecture (clean architecture)

Dependency direction points inward toward `internal/domain`. Wiring happens manually in
`cmd/main.go` (config → DB → repos → usecases → `App`).

- `internal/domain/entity` — core types: `User`, `Moment`, `BoardMessage`, `Activity`,
  `Comment`, `Friend`, `login`. These are also the GORM models (registered in
  `pkg/database/migration.go`).
- `internal/domain/repository` & `internal/domain/usecase` — **interfaces only**.
- `internal/usecase` — usecase implementations (business logic).
- `internal/infrastructure/persistence` — GORM repository implementations.
- `internal/infrastructure/qzone_api` — HTTP client for QQ's endpoints (login, user info,
  activity feed scraping with goquery).
- `internal/infrastructure/config` — Viper config loader. Has hard-coded defaults; an
  optional `config/config.yaml` overrides them. SQLite is the only DB type wired in
  `cmd/main.go` despite the config allowing others.
- `internal/delivery/app` — `App.Run` orchestrates the whole flow.
- `pkg/` — reusable helpers: `database` (DB abstraction + migration), `database/sqlite`,
  `qrcode` (terminal + browser QR display), `utils` (token/auth math, HTML cleanup).

### The `App.Run` pipeline (`internal/delivery/app/app.go`)

1. Check local login (cached cookies in DB); if expired/missing, fetch QR code and serve it
   in the browser via `qrcode.OpenInBrowser` (local ephemeral HTTP server that polls
   `/status` and self-closes on success).
2. Poll QR login status until success, then `CompleteLogin` → persist user + cookies.
3. `activityUseCase.FetchActivities` — pull the entire activity feed.
4. `reconstructionUseCase.ReconstructMomentsFromActivities` and
   `...ReconstructBoardMessagesFromActivities` — infer posts from activities.
5. `exportUseCase.ExportUserDataToJSON` → writes `<QQ>_export.json`.

   Excel/HTML export are stubs that return "not implemented".

## QQ-specific gotchas (apply to both programs)

- **`g_tk` token**: derived from the `p_skey` cookie via the DJB-style hash in
  `utils.GenerateGTK` / `oneclick`'s `genGTK`. Required on most authenticated requests.
- **`ptqrtoken`**: derived from the `qrsig` cookie (`utils.GeneratePtqrToken`) for login
  polling.
- **`uin`**: the QQ number, stored in the `uin` cookie prefixed like `o0...`; strip the
  leading `o` and zeros (`utils.ExtractUin`).
- Login status is detected by **matching Chinese substrings** in the response body
  (`二维码未失效`, `二维码认证中`, `二维码已失效`, `登录成功`) — do not "normalize" these strings.
- Activity feed has no count endpoint: the main app finds the total via **binary search**
  (`getActivityCount`) then pages with a fixed `count=100` and a 200ms delay per page.
- Activities are returned as **HTML**, cleaned by `utils.ProcessOldHTML` and parsed with
  goquery selectors (`li.f-single.f-s-s`, etc.); the markup/class names are the contract.
- `Moment` identity is an **MD5 of `Content + UserQQ`** (`generateMomentKey` and
  `Moment.BeforeCreate`), used to dedup/merge reconstructed moments. Content changes change
  the ID.
- Slice/map fields are persisted with `gorm:"serializer:json"` (e.g. `Moment.ImageURLs`,
  `User.Cookies`).
- `oneclick` strips thumbnail size params to fetch original images and downloads them with a
  `Referer` header to defeat hotlink protection before base64-embedding. Each image keeps
  multiple candidate URLs (`imageCandidates`); download tries them in order with retry/backoff
  (`downloadImageCandidateWith`). Images that fail every candidate are **dropped**, not
  rendered as broken remote `<img>` tags.
- `oneclick`'s feed parsing is hand-rolled regex/string code, not goquery, and must stay
  **CSS-class-order-independent** (`f-s-s f-single` vs `f-single f-s-s`) and decode JS escapes
  (`\xNN`, `\t`, `\n`, `\/`) via `unescapeJS` — a regression here produced the past "全是 ttttt
  乱码" and "一页只解析出 1 条" bugs that `main_test.go` now guards. A QQ's JSONP page can carry
  multiple `html:'...'` segments; `processOldHTML` must concatenate **all** of them.
