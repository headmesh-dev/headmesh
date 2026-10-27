---
name: xcelsior-provider
description: Help a GPU owner who provides capacity on Xcelsior Compute understand their provider account, hosts, earnings, payouts and reputation through the xcelsior MCP server. Use when the user asks about hosting GPUs on Xcelsior, provider onboarding, earnings, payouts, PayPal status or trust tiers.
---

# Providing GPUs on Xcelsior

A provider connects their own GPU machines to Xcelsior and earns when renters use them. Through this plugin you can **read** everything a provider needs. Actions that change provider state or move money happen in the Xcelsior dashboard, because they need scopes that MCP connectors can't hold.

## Read the account

| Question | Tool |
|---|---|
| Is this user a provider, and what is the account's state? | `get_provider_account` |
| What have they earned, and over what period? | `get_provider_earnings` |
| Is their PayPal payout account connected and verified? | `get_paypal_status` |
| What is their reputation, and how is it calculated? | `get_my_reputation`, `get_reputation_breakdown`, `get_reputation_history` |
| What do they need to reach the next trust tier? | `get_trust_tiers`, `get_reputation_journey` |
| How do they compare with other providers? | `list_providers`, `get_reputation_leaderboard` |

Quote amounts in CAD and say which period they cover. When earnings look low, compare them with the host's reputation and trust tier, since placement favours higher tiers, before suggesting price changes.

## Onboarding a new provider

1. Call `get_provider_account`. If the user isn't a provider yet, registration happens in the dashboard. Send them to https://xcelsior.ca/dashboard/hosts.
2. Adding a machine runs Xcelsior's installer on that machine. It enrolls the host into Xcelsior's private network and installs the worker. Walk the user through the dashboard's "Connect your GPU" steps, and have them run the installer themselves. Don't run it for them from another machine.
3. Connecting PayPal for payouts happens in the dashboard too. Check the result afterwards with `get_paypal_status`.

## Payouts and milestones

`request_provider_payout` and `claim_reputation_milestones` need provider and reputation write scopes that this plugin's connection can't hold. If the user wants a payout or a milestone reward, show them what is available from `get_provider_earnings` or `get_reputation_journey`, then send them to https://xcelsior.ca/dashboard/earnings.

## Hosts

Detailed host state (online status, GPUs, running workloads) is in the dashboard at https://xcelsior.ca/dashboard/hosts. Host administration tools such as `drain_host` are operator-only and not available here. If a provider needs a host drained for maintenance, they do it from the dashboard.
