# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Go webhook (Vercel Function) that mimics Notion's "auto-fill": when a database page's title or Description changes, it asks Claude Haiku (`claude-haiku-5-5`) for a category and writes it to a `select` property. README.md (setup, deploy, pause) and CONTEXTO.md (decisions, current state, next steps; in Portuguese) hold the details. Read CONTEXTO.md before resuming work.

## Commands

```bash
go build ./...
go test ./...                                  # add -race (see project rules)
go test ./pkg/webhook -run TestName            # single test
set -a; . ./.env; set +a; go run ./cmd/local   # local server on :8080/api/webhook (use ngrok for Notion)
vercel --prod                                  # deploy; env var changes only apply after a redeploy
```

## Architecture

- `api/webhook.go`: thin Vercel entrypoint (`Handler`) delegating to `pkg/webhook`. `cmd/local` runs the same handler locally.
- `pkg/webhook/handler.go`: the whole flow. Verify the HMAC signature (`X-Notion-Signature`, skipped until `NOTION_WEBHOOK_SECRET` exists), then pause/dry-run checks, fetch the page, run the anti-loop filter (`touches()`), read categories, classify, and write the select.
  - Anti-loop: on `page.properties_updated`, only proceed if `updated_properties` contains the title or Description property ID. Our own Category write is therefore ignored.
  - Transient failures (429/5xx/timeout) return 503 so Notion redelivers. Config errors are logged and return 200 to stop retries.
  - `WEBHOOK_PAUSED=true` returns 200 and drops events. They are not queued.
  - Fallback: a "no fit" answer or `ErrBadAnswer` writes `NOTION_FALLBACK_CATEGORY` (default `Não encontrado`).
- `pkg/classifier`: `Classify` calls Haiku with structured output (an `enum` of the options, so the answer is always valid). `Estimate` only calls `count_tokens`. Input tokens are exact and output tokens are estimated.
- `pkg/notion/client.go`: minimal hand-written HTTP client (`Notion-Version: 2026-03-11`) with rate limit and retry on 429/503.
- `pkg/usage`: writes one MongoDB document per event, with the Notion event ID as `_id` so redeliveries overwrite. Write failures only log. It is disabled without `MONGODB_URI`.

Categories are not in the code. They come from the select options (and option descriptions) in the Notion data source schema on every event.

`DRY_RUN=true` skips Haiku and the Notion write and records estimated usage only. This is the current rollout phase.

## Project rules

`.claude/rules/concurrency/` is mandatory. Run independent work in parallel with `errgroup` and bound it with `SetLimit` and a named constant. Throttle every external API with one shared `golang.org/x/time/rate` limiter created in the client constructor. Honor 429 and `Retry-After`, with bounded retries that also pass through the limiter. Pass `context.Context` first and never use fire-and-forget goroutines.
