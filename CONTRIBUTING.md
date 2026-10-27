# Contributing

1. `mise install`, then `mise run generate test lint` must pass before you push.
2. Commits follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat(core): …`, `fix(cli): …`);
   release-please builds the changelog from them.
3. Every behaviour change needs a test. Changes under `core/policy` also need the property tests to keep passing
   and, if they change emitted policy, updated golden files (`go test ./core/policy -update`).
4. Every HTTP route declares a required permission; `api` tests fail if one does not.
5. No secrets in code, fixtures or logs. `gitleaks` runs in CI and as a pre-commit hook.
6. Dependencies are pinned; new ones need a short justification in the PR. Renovate waits three days after a
   release before proposing it.

Architecture decisions live in `docs/adr/`. Add one when you change something structural.
