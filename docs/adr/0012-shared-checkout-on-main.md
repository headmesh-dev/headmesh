# ADR 0012: parallel tracks share one checkout on main

- Status: accepted (2026-10-27)
- Plan: §3.3

## Decision

Work happens directly on `main` in `/mnt/storage/projects/headmesh`, matching the blueprint's git workflow: no
worktrees or branches unless Aaryn asks for one. Each track edits and commits only its own paths in small commits,
runs `git pull --rebase` before every push, never force-pushes, and never reformats another track's files. When two
agents work at once, they say so and ask before overwriting changes they didn't make.

## Consequences

A track that needs a change in a shared root file (the pnpm workspace, `.gitignore`, `go.mod`) asks through a
hand-over note instead of editing it.
