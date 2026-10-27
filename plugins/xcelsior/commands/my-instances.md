---
description: List your Xcelsior instances with their status, GPU and cost so far
argument-hint: "[instance id or name for details]"
allowed-tools: mcp__plugin_xcelsior_xcelsior__list_instances, mcp__plugin_xcelsior_xcelsior__get_instance, mcp__plugin_xcelsior_xcelsior__get_wallet_balance
---

Show the user their Xcelsior instances. This is read-only: don't stop, cancel or change anything.

Arguments: `$ARGUMENTS`

- **No arguments:** call `list_instances`, following its `cursor` until every page is read, and `get_wallet_balance`. Show running and queued instances first, in a table:

  | Name | ID | Status | GPU | Pricing | CAD/h | Running for | Cost so far |
  |---|---|---|---|---|---|---|---|

  Then list stopped or terminated instances from the last few days in a short line each, and finish with the wallet balance and the total hourly burn of everything still running.

- **An instance ID or name:** call `get_instance` for it (look it up with `list_instances` first if you were given a name) and show its full status, host, access details and timeline.

If something is running that the user may have forgotten, point it out and say that stopping it needs their confirmation through the `xcelsior-gpu-rental` skill. Don't stop it from here.
