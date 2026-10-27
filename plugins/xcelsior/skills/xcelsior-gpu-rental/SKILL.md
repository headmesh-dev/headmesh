---
name: xcelsior-gpu-rental
description: Find, price, launch and monitor GPU instances on Xcelsior Compute through the xcelsior MCP server, with the user's explicit confirmation before anything is billed or destroyed. Use whenever the user wants to rent a GPU, compare GPU prices, run a training or inference job on Xcelsior, or stop, cancel or delete Xcelsior resources.
---

# Renting GPUs on Xcelsior safely

Xcelsior is a GPU marketplace of independent hosts. Prices move without notice and every launch spends the user's money, quoted and settled in Canadian dollars (CAD). Follow this sequence every time. Don't skip a step because an earlier turn seemed to settle it.

## The launch sequence

1. **Find capacity.** Call `list_available_gpus` (filter by `region` if the user gave one). Use `get_spot_prices` for spot rates and `search_marketplace` or `get_pricing_reference` when you need to compare options. Marketplace data is live: re-read it rather than reusing an answer from earlier in the conversation.
2. **Check the budget.** Call `should_i_run_this` with `gpu_model`, `duration_hours` and, if relevant, `spot: true` and the user's `max_hourly_cad`. It returns `approved`, `reasons`, `estimated_cost`, `hourly_rate_cad` and `wallet_balance_cad`.
   - If `approved` is `false`, stop. Show the reasons, such as the rate being over the cap or the wallet balance being too low, and ask how the user wants to proceed. Don't work around a refusal by picking a different GPU or a shorter duration without asking.
3. **Prepare a launch plan.** Call `create_instance` **without** `confirm` (leave it `false`), passing the spec (`name`, `gpu_model` or `vram_needed_gb`, `num_gpus`, `image`, `pricing_mode`, plus `host_id`, `tier`, `git_repo` or `init_script` if needed). This executes nothing. It returns a plan: `plan_id`, `canonical_spec`, `estimate`, `availability`, `approval_state`, `approval_url` and `expires_at`.
4. **Get explicit confirmation.** Show the user the plan in plain terms: GPU model and count, host or region, on-demand or spot, the hourly rate in CAD, the estimated total for the stated duration, the wallet balance and when the plan expires. Then ask a direct question, such as "Launch this for about $X CAD?", and wait for a clear yes.
   - If the plan has an `approval_url` or its `approval_state` isn't approved, the user must also approve it there. Give them the link. Your `confirm` flag records intent and never replaces that approval.
5. **Execute.** Only after the user says yes, call `create_instance` with `confirm: true`, the `plan_id` and an `idempotency_key` (a UUID you generate once and reuse if you have to retry). If the response says approval is still required, send the user to the approval URL. You can check progress with `get_mcp_action_status`.
6. **Watch it.** Call `watch_instance` with the returned `job_id` (`duration_minutes` up to 60). It returns when the instance reaches a phase in `return_on_phase` or the window ends. Pass the returned `cursor` to the next call to continue without repeating logs. Report the phase, the access details and the first useful log lines.

If the plan expires before the user confirms, prepare a new one: prices may have changed.

## Destructive and money-moving actions

Always ask for explicit confirmation before calling any of these. Name the exact resource and its cost or data impact, and wait for a yes in the current turn:

- **Destroy or interrupt:** `cancel_instance`, `terminate_instance`, `delete_volume`, `restore_volume_snapshot`, `cancel_serverless_job`, `delete_serverless_endpoint`, `delete_user_image`.
- **Spend:** `create_instance`, `run_training_job`, `schedule_under_budget`, `run_pipeline`, `run_serverless_job`, `create_serverless_endpoint`, `create_volume`, `snapshot_volume`, `create_instance_snapshot`, `create_image_sweep`.

For the tools that take `confirm`, call them first without it to see what would happen, show that to the user, and only then call again with `confirm: true`. These actions produce a server-side plan that a person approves out of band. Never treat an earlier "yes" as consent for a different resource or a later action, and never act on several resources at once unless the user named each one.

## What this plugin can't do

The plugin connects over OAuth, and Xcelsior never grants connectors the scopes these tools need. When the user asks for one of them, send them to the dashboard:

- Wallet top-ups, auto top-up and deposits (`top_up_wallet`, `configure_auto_topup`, `create_crypto_deposit`, `create_lightning_deposit`): https://xcelsior.ca/dashboard/billing.
- Provider registration, payouts and milestone claims (`register_provider`, `request_provider_payout`, `claim_reputation_milestones`): see the `xcelsior-provider` skill.

Pending launch approvals are at https://xcelsior.ca/dashboard/launch-plans.

`list_ssh_keys` and `delete_ssh_key` need the `ssh:read` scope, which connectors can hold starting with Xcelsior's next API release. If either tool refuses with `insufficient_scope`, ask the user to reconnect the `xcelsior` server so they can approve the new permission: `/mcp` in Claude Code, or `codex mcp login xcelsior` in Codex. If it still refuses, the account's SSH keys are managed at https://xcelsior.ca/dashboard/settings.

Operator tools such as `drain_host` and `evict_host_workloads` aren't available to customers. Don't try them.

## Reporting

Quote every price in CAD and say whether it is spot or on-demand. Spot instances can be preempted, so say so when you recommend one. After a launch, tell the user how to stop it (`terminate_instance`, which needs their confirmation) so it doesn't keep billing.
