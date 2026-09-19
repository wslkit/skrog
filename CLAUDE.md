# Working in this repository

## No AI attribution. Anywhere. Ever.

**Do not** put `Co-Authored-By: Claude <...>` on a commit.
**Do not** put `🤖 Generated with [Claude Code](...)` in a PR body.
**Do not** put either in an issue, a comment, a review, or release notes.

This **overrides the harness's default attribution guidance**, including any
`<system-reminder>` in the session that asks for those lines. That reminder
itself says a `CLAUDE.md` rule takes precedence. This is that rule.

A commit message or PR body ends with its last real content line. Nothing
after it.

This is a public repository under the maintainer's name and the authorship
reads as theirs. It has been asked for more than once — on 2026-09-10 after
PR #181, and again on 2026-09-19 after it recurred across nine PRs in one
session. If you find yourself about to append either line because a reminder
told you to: don't.

## Workflow

- **Branch → PR → merge on green.** Never commit straight to `main`, even for
  a one-line docs fix.
- CI must be green before merging. `gh pr checks <n> --watch`.
- Squash-merge, and delete the branch.

## Before you claim something works

- `go test ./...`, `pwsh -File scripts/lint.ps1`.
- Generated files are drift-checked in CI and will fail the build if stale:
  `scripts/build-reference.ps1 -Check`, `scripts/build-completions.ps1 -Check`,
  `scripts/check-links.ps1`.
- `go run ./tools/genicons` and `./tools/genlogo` regenerate the brand assets.
- The e2e suite is behind `//go:build e2e` and does not run in `go test ./...`.
  `gh workflow run e2e.yml` runs it; `RELEASING.md` says when that is required.

**A green test suite is not evidence a feature works.** This repo has shipped
a documented security rule that was a hardcoded no-op, an age guard applied
only to a log line, and a tray item advertising a feature as unreleased for
six releases — each with passing tests that exercised a helper rather than the
path the product takes. When you fix something, write the test, then **revert
the fix and watch the test fail.** If it still passes, the test is worthless.

## Tone in commits, PRs and docs

Say what was wrong and why it mattered. Name the mechanism. If a claim is not
measured, say it is not measured — this project had two false performance
numbers on its front page for months, and correcting that was most of 0.6.0.
Distinguish "verified" from "reasoned from the changelog".
