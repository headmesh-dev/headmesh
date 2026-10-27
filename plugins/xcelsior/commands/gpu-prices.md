---
description: Show current Xcelsior GPU prices and availability, cheapest first
argument-hint: "[gpu model, minimum VRAM in GB, or region]"
allowed-tools: mcp__plugin_xcelsior_xcelsior__list_available_gpus, mcp__plugin_xcelsior_xcelsior__get_spot_prices, mcp__plugin_xcelsior_xcelsior__get_pricing_reference
---

Show the user current GPU prices on Xcelsior. This is read-only: don't prepare or launch anything.

Filter: `$ARGUMENTS`. This may be a GPU model (such as `RTX 4090`), a minimum VRAM (such as `24`), a region code (such as `ca-east`) or empty.

1. Call `list_available_gpus`, passing `region` if the filter is a region code, and `get_spot_prices`. Call `get_pricing_reference` only if you need on-demand list prices that the other two don't give.
2. Apply the filter, then build one table, cheapest first:

   | GPU | VRAM | Available | On-demand CAD/h | Spot CAD/h | Region |
   |---|---|---|---|---|---|

3. Below the table, give the cheapest option that meets the filter, mention that spot instances can be preempted, and say that prices are live and can change before a launch.

If the user then asks to launch, follow the `xcelsior-gpu-rental` skill from its first step.
