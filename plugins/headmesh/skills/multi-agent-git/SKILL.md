---
name: multi-agent-git
description: The protocol for sharing a git checkout with other agents and with the user. Use whenever you commit, pull or push in a Headmesh sandbox, or whenever sandbox_status, agents_list or git_status shows another agent active in the same repository.
---

# Sharing a repository with other agents

Several agents (Claude, Codex and the user) often work in the same checkout at once. There is no lock by default, so this protocol is what keeps work from being lost.

## Before you start

1. Call `agents_register` with your agent name, a one-line task and the repositories you will touch. While you work, call `agents_heartbeat` every few minutes.
2. Call `agents_list` or `git_status` to see who else is active and on which files.
3. Work on `main`. Create a branch or worktree only when the user asks for one.

## While you work

- **Stay in your own files.** Change only the files your task needs. Don't reformat, re-sort imports or make sweeping edits in files you aren't otherwise changing.
- **Commit small and often.** Make one logical change per commit, with a conventional commit message such as `fix(sandbox): handle lost sessions`. Small commits rebase cleanly past other agents' work.
- **Never overwrite changes you didn't make.** If a file you need has uncommitted or unfamiliar changes, stop and ask the user. Don't stash, reset, check out or delete them, even if they block you.

## Pushing

1. Run `git pull --rebase` (or `git_sync`) before **every** push.
2. If the rebase conflicts in a file you didn't write, stop and ask the user. Resolve a conflict yourself only when every conflicting hunk is your own change.
3. Push with `git_push_safe` or a plain `git push`. **Never force-push `main`**, with `--force`, `--force-with-lease` or a `+refspec`. The sandbox's pre-push hook refuses it anyway, and a refusal means you need to rebase, not override.
4. If the push is rejected because `main` moved, go back to step 1.

## When the repository is locked

If the repository uses `repo_lock = "enforce"`, the pre-commit hook refuses commits while another live agent holds `.git/headmesh-agent.lock`. Wait and retry, or ask the user. Never delete the lock file.

## Telling the user

When you finish, say which commits you pushed and mention any other agent whose work you rebased over, so the user can follow what changed.
