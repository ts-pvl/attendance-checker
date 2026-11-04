# Project instructions

QR presence check-in for a school with ~50 shared Linux PCs. The spec is the
PDF in the repository root; choices not in the spec are in `docs/decisions.md`.

## Stack (do not change without approval)

* Go, standard library first: `net/http`, `html/template`, `embed`, `encoding/csv`.
* SQLite via `modernc.org/sqlite` (pure Go, no cgo). WAL, busy timeout, short write transactions.
* `golang.org/x/crypto/bcrypt` for staff passwords.
* Vendored front-end files in `internal/web/static`: htmx 2.0.11, qrcode-generator 2.0.4. No Node toolchain.
* **No new dependency without approval.** Pin versions in `go.mod`.

## Rules

* **Server clock only.** Never use a time sent by a PC or a phone. Agents send idle durations, not timestamps.
* **Raw events first.** Every input goes into `events` before it changes state, and all state changes go through `core.Engine.apply`. Replaying the events must reproduce sessions and presence exactly (`TestReplayReproducesState`).
* **Corrections never modify `presence`.** They are separate audited rows layered on top by `hours.Apply`.
* **One query for hours.** The Hours screen, CSV export, student page and bot all use `hours.Load` + `Calc.Summarize`.
* **Hours and token rules: tests first.** Expected values in tests are computed by hand from the scenario, never by calling the code under test. When changing a rule, break it on purpose and check that a test fails.
* Every user-visible string lives in `internal/i18n` in both English and Russian.
* Never log or paste the bot token, host key, token secret or real student data. Use a separate test bot and fake data.

## Files a human must read line by line after any change

`internal/token/`, `internal/core/checkin.go`, `internal/core/enroll.go`,
`internal/core/engine.go`, `internal/web/auth.go`, `internal/web/pc.go`,
`agent/idle_logout`.

## Commands

`make test`, `make race`, `make check` (vet, staticcheck, govulncheck, gosec, shellcheck), `make pi`.
