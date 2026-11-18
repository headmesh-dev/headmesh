# Headmesh clients: roadmap to a Tailscale-calibre product

- Status: **proposal** (2026-11-14). Nothing here is decided until the ADRs in §15 are accepted.
- Owner: Aaryn Biro (Xcelsior Compute Inc.).
- Builds on: `docs/plan.md` (§9 core, §10 CLI, §12 web, §13.6 DERP, §14 security), ADR 0004, ADR 0011, ADR 0015.
- Proposed as a new **Track H — Clients** in `plan.md` §4 once accepted. Its phases are numbered **C0–C6** so they don't
  clash with the core milestones M0–M5.

---

## 0. TL;DR

**The goal.** Someone hears about Headmesh, downloads a signed app from `headmesh.dev/download`, signs in through
their browser, and within two minutes their Mac shows a Headmesh icon in the menu bar. The menu lists their devices,
the machines other orgs have shared with them (with time left on each grant), exit nodes, services and agent
sandboxes. It survives sleep, Wi-Fi changes and reboots, and it updates itself. Windows, Linux, iOS and Android follow,
and an admin console handles the rest. It is clearly its own product, not a reskinned Tailscale.

**How to get there from here.** Headmesh already has the hard, rare part: a multi-tenant control layer with property-tested
isolation, grants that revoke in under 5 s, and a tamper-evident audit log. It has no client of its own yet: macOS
users are told to install Tailscale's app and point it at a custom server. The roadmap closes that gap in seven
phases:

| Phase | What ships | Rough duration\* |
|---|---|---|
| **C0 Foundations** | ADRs; a `clients/` Go module; `headmeshd`, a daemon built on the upstream `tailscale.com` library with Headmesh defaults and no traffic to Tailscale's servers; Apple and Windows signing accounts | 2–3 weeks |
| **C1 Identity and product plane** | Orgs and members in the API; native-app OAuth (PKCE plus device flow); **personal devices owned by a person**; device registration, client config, overview and event-stream endpoints | 3–5 weeks (overlaps M3) |
| **C2 macOS alpha** | Menu bar app plus Network Extension (system extension); sign-in; device list; notarized DMG with Sparkle updates; dogfooding | 4–6 weeks |
| **C3 macOS beta + console + downloads** | Exit nodes, MagicDNS, file sharing, grants and access requests in the menu, several orgs, MDM; admin console v1; download page and Homebrew cask | 4–6 weeks |
| **C4 Windows + Linux** | Windows service plus tray app, signed MSI, auto-update, winget; Linux tray app with apt/yum repositories | 6–8 weeks (can start after C1) |
| **C5 Mobile** | iOS (shared SwiftUI, TestFlight, then the App Store); Android (fork of the BSD-licensed `tailscale-android`) | 8–12 weeks |
| **C6 GA and differentiation** | Own DERP regions, HTTPS certificates, public links, device posture, peer relays, enterprise SSO, Terraform, a GitHub Action | ongoing |

\* One full-time engineer with coding agents, plus a part-time Apple-platform specialist for C2. That gives roughly
**5–6 months to macOS, Windows and Linux GA**, and **9–12 months** to mobile and broad feature parity.

**The five decisions that shape everything** (details in §14):

1. **Own the shell, borrow the engine.** Build Headmesh's own apps on the upstream `tailscale.com` Go library, unforked
   and pinned to release tags. Don't fork Headscale, don't invent a protocol, don't reimplement WireGuard.
2. **Personal devices become user-owned nodes**, with one Headscale user per (person, org). Servers and sandboxes
   stay tagged. This amends ADR 0011 and needs property tests and e2e (isolation is the product).
3. **macOS first**, as a Developer ID app with a Network Extension packaged as a system extension (the same shape as
   Tailscale's recommended "Standalone" variant), on macOS 14 and later.
4. **The app talks to two planes.** Headscale (via the engine) carries packets, peers and DNS. The Headmesh API
   carries everything that makes Headmesh different: orgs, grants, services, sandboxes and requests. Neither the
   Tailscale protocol nor Headscale is extended.
5. **Public downloads, sign-in in the app.** Anyone can install. Nothing works until the user signs in. An invite link
   opens the app directly, or the download page if the app isn't installed.

---

## 1. What "Tailscale calibre" means

Tailscale's quality bar is mostly invisible work. Concretely, the bar is:

| Area | The bar |
|---|---|
| **Install** | One signed, notarized download per OS (or a store listing); installed in under 60 s; no terminal |
| **Sign-in** | "Sign in" opens the system browser; SSO (GitHub, Google, Microsoft, passkey, email link; enterprise OIDC/SAML later); back in the app automatically |
| **First connection** | The device appears in the admin console and in every peer's list within seconds; a readable name and a stable IP |
| **Everyday use** | Menu bar or tray presence; one-click connect and disconnect; device list with copy-IP, copy-name, SSH, send file; exit node picker; notifications that matter and nothing else |
| **Robustness** | Survives sleep and wake, Wi-Fi to Ethernet, captive portals, VPN conflicts, reboots and OS upgrades without user action. Direct connections when possible, relays when not |
| **Updates** | Silent, signed auto-updates with beta and stable channels and staged rollouts |
| **Management** | Admin console; device approval; key expiry and re-auth; MDM and Group Policy settings; audit log; API |
| **Platforms** | macOS, Windows, Linux, iOS, Android and headless servers |
| **Trust** | Open code, signed builds, a clear privacy policy, no surprise telemetry, a security page, responsive support |

**"Different product".** Headmesh must look and behave like Headmesh: its own name, icons, copy and interaction
design. It may say factually that it uses open-source Tailscale code and works with Headscale. It must not use
Tailscale's names or logos in the UI, binaries, bundle IDs or docs, and must not copy Tailscale's UI or assets. What
makes it different is what Tailscale doesn't do:

- **Sharing between organizations is the core feature**, not an add-on. Machines are shared for a set time and
  revoke themselves; the menu shows the time left.
- **Agent sandboxes** are first-class devices with modes (Regular / YOLO) visible in the menu.
- **Open control plane** (AGPL) that anyone can self-host, and **open-source GUI clients on every platform**
  (Tailscale's macOS, iOS and Windows GUIs are closed source).
- **Several networks at once** on one machine (already true on Linux through isolated `tailscaled` instances).
- **Tamper-evident audit log** with offline verification.

---

## 2. Where Headmesh is today

| Area | State (2026-11-14) |
|---|---|
| Policy compiler, verifier, property tests | **Built.** Grants-only output, isolation canaries as Headscale `tests`, independent verifier (ADR 0011, 0014) |
| Cell agent (outbound mTLS, unix socket, local guardrails) | **Built** (ADR 0004) |
| Enrollment | **Built** for tagged devices: `hmj_` join tokens → single-use pre-auth key with `acl_tags` → `tailscale up` |
| Grants, rules, services, access check | **Built.** Revocation p99 < 5 s; established flows cut in about 1 s (T-13) |
| Audit log | **Built** (hash chain, export, offline verify); daily anchor still to do |
| API | `/v1` live, but **the org comes from an API key**. No orgs/members endpoints, no user-scoped access |
| Browser sign-in | **Built** for GitHub and Hugging Face (identity only; no membership) |
| OAuth authorization server | **Built for MCP** (scoped consent, rotation). No first-party native clients, **no device flow yet** |
| CLI | `join`, `devices`, `invite`, `grant`, `rule`, `service`, `access`, `audit`. **No `login`, `up`, `status`** |
| Linux client | Drives the stock `tailscaled`; isolated instances (`headmesh-tailscaled@<name>`) for coexistence |
| macOS client | **None.** Docs point to Tailscale's app with a custom login server |
| Windows client | **None** (plan says M5+) |
| Mobile | **None** |
| Web app / admin console | **Not started** (`web/app` is in the plan, not the repo) |
| Device identity | **Every node is tagged** (ADR 0011): no person attached, no key expiry |
| DERP | Embedded region 900 on hm-prod-1; second location undecided (`plan.md` §19.6) |
| Headscale | Pinned to **v0.29.6**; **v0.29.7 (patch and security release) shipped 2026-10-30** and should be adopted first |
| Tailscale clients in tests | `tailscale:v1.104.2` in CI; upstream is at **v1.104.7** (2026-11-12) |

The gap is almost entirely on the client side and in the user-facing half of the API. The isolation core needs one
real change (person-owned devices, §4.6) and one careful extension (exit nodes, §7.2).

---

## 3. The core decision: what the client is

### 3.1 Options

| Option | What it is | Effort | Brand | Features | Verdict |
|---|---|---|---|---|---|
| **A. Official Tailscale apps** pointed at a Headmesh cell | Status quo for macOS. Users install Tailscale and enter a custom server | None | Tailscale's | Bounded by Headscale; nothing Headmesh-specific in the UI; the Windows GUI can't even set a custom server (registry or CLI only) | Keep as **"bring your own client"** forever. It costs nothing because Headscale speaks the protocol |
| **B. Companion app** that drives someone else's daemon | A menu bar app on top of Tailscale's app or daemon | Low | Mixed: two menu bar icons | Fragile; depends on another vendor's closed app | Rejected |
| **C. Own client on the upstream `tailscale.com` library** | Headmesh's own GUIs and daemon; the networking engine is upstream Tailscale code as a pinned Go dependency | High, but bounded | Headmesh's | Everything Headscale supports, plus Headmesh features through its own API | **Recommended** |
| **D. Clean-room engine** | New WireGuard/NAT-traversal stack or a new control protocol | Very high | Headmesh's | Starts from zero | Rejected: breaks "don't reimplement networking or cryptography" (README) and loses Headscale |

### 3.2 What option C means precisely

- **No fork of the `tailscale/tailscale` repository.** `tailscale.com` is a normal Go module dependency pinned to a
  release tag (today v1.104.x). Headmesh writes its own `main` packages and a thin wrapper that sets its defaults.
  Patches are a last resort; any patch is kept in a short queue and offered upstream.
- **Headmesh defaults, no phoning home.** Upstream defaults point at Tailscale's infrastructure, and each one must be
  changed and tested:
  - control URL: `ipn.DefaultControlURL` is `https://controlplane.tailscale.com`. Headmesh sets the cell's URL from its
    own discovery (§4.7) and never falls back to Tailscale's;
  - logs: the engine uploads logs to `log.tailscale.io` by default. Build with the `ts_omit_logtail` feature tag, or
    set `TS_LOG_TARGET` to a Headmesh log endpoint (§10);
  - update checks (`clientupdate`) go to `pkgs.tailscale.com`; disable them and use Headmesh's updater;
  - DERP comes from the control server's DERP map, so it is whatever the cell advertises (§9.4).
- **A CI check proves it.** Run the client in a network namespace with an egress allowlist (the cell, Headmesh's API,
  Headmesh's DERP, STUN) and fail the build on any connection to a Tailscale-owned host.
- **Versions.** The engine version follows upstream security releases within days. Headscale only accepts clients in
  its supported capability-version window (`hscontrol/capver`), so CI tests every client build against every Headscale
  version the cells run (§9.5).
- **Licences.** `tailscale/tailscale` and `tailscale/tailscale-android` are BSD-3-Clause: keep their copyright notices
  in an "Open-source licences" screen and in `NOTICE`. `wireguard-apple` (MIT) is the best open reference for the
  Swift ↔ Go Network Extension bridge. Headmesh's own client code is Apache-2.0: it falls under "everything not
  listed" in `LICENSING.md` and must never import `api/`, `core/`, `internal/` or `db/`.
- **A separate Go module.** `clients/` gets its own `go.mod`, as `sandbox/` does, so the large `tailscale.com` dependency tree
  stays out of the server module, the client pins its engine independently, and the licence boundary is
  structural.

---

## 4. Target architecture

### 4.1 Layers

```
 ┌──────────────────────────── GUI shells (thin; render state, send intents) ────────────────────────────┐
 │ macOS menu bar app   iOS app      Windows tray      Linux tray       Android app      headmesh CLI    │
 │ (SwiftUI)            (SwiftUI)    (Go)              (Go)             (Kotlin/Compose) (Go)            │
 └────────────┬──────────────┬─────────────┬───────────────┬──────────────────┬─────────────┬────────────┘
              │ LocalAPI over the platform transport: NE provider messages / unix socket / named pipe / JNI
 ┌────────────▼──────────────▼─────────────▼───────────────▼──────────────────▼─────────────▼────────────┐
 │ Headmesh client core (Go, clients/)                                                                   │
 │  • sdk: OAuth (PKCE, device flow), token store, discovery, typed Headmesh API client, SSE events      │
 │  • overview: merges netmap peers with Headmesh devices, grants, services, sandboxes (by node ID)      │
 │  • localapi: Headmesh extensions served next to upstream LocalAPI (/localapi/v0/headmesh/*)           │
 │  • engine: tailscale.com ipnlocal + wgengine + magicsock + net/dns + tstun, with Headmesh defaults    │
 └────────────┬───────────────────────────────────────────────┬─────────────────────────────────────────┘
              │ ts2021 (Noise) control + WireGuard data        │ HTTPS: OAuth, /v1/*, SSE
 ┌────────────▼──────────────────┐   ┌────────────────────┐   ┌▼──────────────────────────────────────────┐
 │ Headscale cell (hs1, xcelsior)│   │ DERP + peer relays │   │ headmesh-server (API, OAuth, console)     │
 │ unexposed API, cell agent ────┼───┼────────────────────┼──►│ policy compiler → cell agent → Headscale  │
 └───────────────────────────────┘   └────────────────────┘   └───────────────────────────────────────────┘
```

### 4.2 Two planes, merged in the client

- **Network plane (Headscale, via the engine):** peers, IPs, online state, endpoints, DNS, routes, SSH policy and
  capabilities. This is what the Tailscale protocol already carries. Headmesh shapes it only through the policy it
  compiles.
- **Product plane (Headmesh API):** org names and roles, which devices are mine, org or shared, grants with
  expiry, services with names and ports, sandbox status, access requests, notifications, client config, minimum
  versions.
- **The merge key** is the node ID: tailcfg's node ID equals Headscale's node ID, which Headmesh already stores as
  `devices.headscale_node_id`. A peer the netmap shows but Headmesh doesn't know is shown plainly. A Headmesh
  device missing from the netmap is shown as unreachable or offline, never as an error.
- **Why not extend the protocol:** a Headscale fork is ruled out (ADR 0011), and the product plane changes on
  Headmesh's schedule, not Headscale's.

### 4.3 Where the engine runs, per platform

| Platform | Engine host | Data path | GUI ↔ engine transport | Notes |
|---|---|---|---|---|
| **macOS (ship)** | Network Extension `NEPacketTunnelProvider` packaged as a **system extension** | utun from the packet flow | Provider messages (`sendProviderMessage`) carrying LocalAPI requests, plus a status stream | Developer ID distribution needs the `packet-tunnel-provider-systemextension` entitlement (§5.6) |
| macOS (dev, servers) | `headmeshd` launchd daemon (upstream tailscaled code path) | `/dev/utun` | unix socket | Runs before login. Upstream marks this variant as partial (can't *use* exit nodes, Taildrop incomplete). Fallback if the NE spike slips |
| macOS App Store (later) | NE app extension (sandboxed) | utun | Provider messages | Same code, different target |
| **iOS / iPadOS** | NE app extension | utun | Provider messages | Packet tunnel extensions have a tight memory ceiling (about 50 MB): build with `ts_omit_*` tags |
| **Windows** | `headmeshd` Windows service | Wintun (the signed driver upstream already uses) | Named pipe | Tailscale's Windows GUI is closed, so the tray app is new code |
| **Linux** | `headmeshd` systemd service, plus named instances | `/dev/net/tun` | unix socket | Generalizes `headmesh-tailscaled@.service`, which already exists |
| **Android** | `VpnService` | TUN fd | In-process (gomobile) | Fork of `tailscale-android` (BSD-3, Kotlin + Compose + Go) |

**Rule:** every GUI is written against a `LocalClient` interface (status, prefs, profiles, watch bus, Headmesh
overview). Moving macOS from the daemon to the system extension, or adding the App Store build, must not touch UI
code.

### 4.4 The local API

- Upstream LocalAPI (status, prefs, login, logout, profiles, IPN bus watch, exit node suggestions, file targets,
  `ping`, `whois`, `cert`, `debug`) is reused as is.
- Headmesh adds `/localapi/v0/headmesh/*`, served by the same process: `overview`, `orgs`, `grants`,
  `access-requests`, `services`, `sandboxes`, `notifications`, `diagnostics`. The client core calls the Headmesh API
  and caches, so every GUI and the CLI share one implementation and one token store.
- Tokens live where the engine runs: a shared Keychain access group on Apple platforms, DPAPI on Windows,
  libsecret or a 0600 file on Linux, EncryptedSharedPreferences on Android.

### 4.5 Sign-in and enrollment (no Headscale page, ever)

```
 App                      Browser                 headmesh-server             cell agent → Headscale
  │ 1 Sign in ───────────► authorize (PKCE S256,   │                           │
  │                        client headmesh-desktop)│                           │
  │                        2 GitHub / Google / HF / passkey / email link      │
  │                        3 choose or create org  │                           │
  │ ◄── 4 code via headmesh:// (Apple) or loopback (Windows/Linux, RFC 8252)  │
  │ 5 token exchange ────────────────────────────► │ access 15 min + rotating refresh (reuse detection)
  │ 6 POST /v1/devices/register {org, hostname, os, kind:"personal"} ─────────►│ EnsureUser(person, org)
  │                                                │ ◄── single-use pre-auth key, 5 min, user-bound
  │ ◄── {login_server, auth_key, device_id} ───────│                           │
  │ 7 engine logs in to Headscale with the key (ts2021) ──────────────────────►│ node registered
  │ 8 POST /v1/enrollments/{id}/complete ─────────►│ verify key id, rename, audit, webhook, SSE
```

- The **Headscale API stays unexposed** (ADR 0004): only the cell agent mints keys, as today.
- **Headless machines** use the device flow (RFC 8628): `headmesh login` prints a code and URL.
  **Servers and sandboxes** keep `hmj_` join tokens and stay tagged.
- **Re-auth:** when a personal device's key nears expiry, the app runs steps 5–7 again in the background if the
  refresh token is still valid, and otherwise asks the user to sign in. Spike: confirm that Headscale keeps the same
  node when a known machine key logs in with a new pre-auth key, and how it sets expiry on pre-auth-registered user
  nodes.
- **Self-hosters:** "Use another Headmesh server…" in the app fetches `https://<host>/.well-known/headmesh`
  (issuer, API base, OAuth endpoints, minimum client version) and shows a clear warning that this server will control
  the device's network.

### 4.6 Device identity: person-owned devices (the hardest backend change)

Today every node is tagged (ADR 0011), which is right for servers, GPU hosts and sandboxes, but loses things a laptop
or phone needs:

- In Tailscale semantics, tagged nodes have **no user identity**: `whois` returns tags, so identity headers in Serve,
  SSH user mapping and "my devices" don't work.
- **File sharing between your own devices** (Taildrop) works only between nodes owned by the same user.
- **Key expiry** doesn't apply, so a lost laptop keeps access until someone removes it.

| Option | How | Pros | Cons |
|---|---|---|---|
| (a) Stay all-tagged; add `tag:hm-u-<user10>` | Person identity lives only in Headmesh | Smallest compiler change; no expiry handling | Same-user features, identity headers and expiry still missing; the client fakes "my devices" |
| **(b) One Headscale user per (person, org)** | Pre-auth key with a user (`hm-u-<user10>-<org10>`) and no tags; display name and email copied from Headmesh | Every same-user feature works; org isolation stays exact, because one Headscale user never spans two orgs; maps 1:1 onto client profiles (one per org) | Compiler must handle user principals beside tags; the cell agent needs an `EnsureUser` op; expiry and re-auth to build |
| (c) One Headscale user per person | One user across orgs | Fewest users | A person in two orgs in one cell would carry org A's access onto their org B laptop, so rules would need per-device IPs. **Rejected** |

**Recommendation: (b).** Personal devices are user-owned; infrastructure stays tagged.

What it takes:

- **ADR amending 0011.**
- **Compiler:** an org's principal set becomes "org tags ∪ that org's Headscale users". I1–I7 are restated over both
  kinds, generators produce mixed tagged and user-owned nodes, and the verifier re-derives cross-org edges for both.
  `autogroup:self` and `autogroup:member` stay out of tenant output unless they are proven cell-safe.
- **Cell protocol:** `EnsureUser{name, display_name, email}` and `CreatePreAuthKey{user, …}`; guardrails extended
  (users can't be deleted while they own protected nodes).
- **Migration:** `users ↔ orgs` memberships drive `hm-u-*` users per cell; `devices.kind = personal` with
  `owner_user_id`.
- **e2e:** the isolation matrix with mixed node kinds, plus same-user Taildrop working and cross-org Taildrop failing.
- **Org setting:** key expiry for personal devices (default 180 days; tagged devices never expire, as today).

### 4.7 Discovery and cells

The client never hard-codes cells. `GET /v1/client/config` returns, per org the user belongs to: the login server
(the cell's `server_url`), MagicDNS suffix, feature flags (exit nodes allowed, SSH, file sharing), DERP policy,
`min_client_version` and `recommended_client_version`. Each org becomes one engine **login profile**, so switching
orgs uses the engine's own fast profile switching.

### 4.8 Product-plane endpoints the clients need

| Endpoint | Purpose |
|---|---|
| `GET /.well-known/headmesh` | Discovery for self-hosted servers |
| `GET /v1/me`, `GET /v1/me/orgs` | Who am I, which orgs and roles |
| `POST /v1/orgs`, members and invites (`plan.md` §9.8) | Personal org on first sign-in; invite teammates |
| `POST /v1/devices/register` | User-authenticated enrollment of a personal device (§4.5) |
| `GET /v1/client/config` | Per-org login server, flags, version gates (§4.7) |
| `GET /v1/client/overview` | One call for the menu: my devices, org devices, shared with me (grants in, with expiry), shared by me, services, sandboxes, pending requests |
| `GET /v1/events/stream` (SSE, planned in §9.8) | Live updates: device online/offline, grant created/expiring/revoked, request approved |
| `POST /v1/access-requests`, `:approve`, `:deny` | **New.** Request time-limited access from the menu; the grantor's admins approve in the app, console or by webhook; approval creates a grant |
| `POST /v1/devices/{id}/posture` | **New, C6.** Client-reported posture (§7) |
| `POST /v1/devices/{id}/cert-challenge` | **New, C6.** DNS-01 challenge for HTTPS certificates (§7) |
| `POST /v1/diagnostics` | Opt-in, redacted bug-report upload |

All of them follow the existing conventions (Huma, OpenAPI 3.1 committed, oasdiff gate, scopes, rate limits, audit).

---

## 5. macOS app (the flagship)

### 5.1 Bundle

- `Headmesh.app` (SwiftUI, macOS 14+), bundle ID `dev.headmesh.macos`.
- `Contents/Library/SystemExtensions/dev.headmesh.macos.network-extension.systemextension`: the
  `NEPacketTunnelProvider` with the Go engine linked in.
- `Contents/MacOS/headmesh`: the CLI, talking to the extension through the app (for `headmesh status`, `ping`, `ssh`).
- Launch at login through `SMAppService.mainApp`. No privileged helper tool.

**Why macOS 14:** `MenuBarExtra` and the Observation framework are mature there, and the support matrix stays small.
Tailscale supports 13, so revisit only if beta users ask.

### 5.2 Go ↔ Swift bridge

- Build the engine as `HeadmeshKit.xcframework`: `go build -buildmode=c-archive` for `arm64` and `x86_64`, merged
  with `lipo`, with a small exported C API: `hm_start(config_json, tun_fd)`, `hm_stop()`,
  `hm_localapi(request) → response`, and a callback for state changes and tunnel-settings changes.
- `wireguard-apple` (MIT) shows the pattern: reading the utun file descriptor from the packet flow, and
  starting and stopping a Go engine inside the extension.
- **Tunnel settings:** `NEPacketTunnelNetworkSettings` from the netmap: IPv4/IPv6 addresses; included routes for peer
  addresses, accepted subnet routes, and the default route when an exit node is on; DNS `100.100.100.100` with
  `matchDomains` for the MagicDNS suffix and split-DNS domains. Re-apply on every netmap change that alters them.
- **Spike C2-S1 (1 week, time-boxed):** extension starts, joins hs1, pings a peer, MagicDNS resolves, survives sleep
  and wake. **Fallback:** ship the alpha on the `headmeshd` launchd daemon (registered with `SMAppService.daemon`)
  and move to the extension in C3.

### 5.3 Menu bar UX (Headmesh's own design)

**Icon:** a monochrome template image drawn from the Headmesh mark, so it follows light and dark menu bars.

| State | Icon | Menu header |
|---|---|---|
| Signed out | Outline mark | "Sign in to Headmesh…" |
| Connecting | Outline mark, gentle pulse | "Connecting to Acme…" |
| Connected | Filled mark | "Connected · Acme" |
| Connected through an exit node | Filled mark + small arrow badge | "Connected · traffic via ops-1" |
| Needs attention | Filled mark + dot badge | The reason: sign-in expires soon, update required, extension not approved, DNS conflict |
| No network | Struck-through mark | "Waiting for network" |

**Menu** (native `NSMenu` through `MenuBarExtra(.menu)` for v1: accessible, keyboard-driven, familiar):

```
● Connected · Acme Robotics                         (⌥ shows cell, DERP region, direct/relayed counts)
  This Mac: aaryn-mbp-acme · 100.64.0.23   [copy]
  Disconnect                                         ⌘D
────────────────────────────────────────────
My devices              ▸  asus-sandbox   ● online  ▸ Copy IP · Copy name · SSH · Send file… · Open in console
Acme devices            ▸  build-box      ● online  ▸ …
Shared with me          ▸  gpu-host-17 (Xcelsior)   ⏱ 3 h 12 m left
                              ▸ Open Jupyter (tcp:8888) · SSH · Ask for more time…
Shared by me            ▸  build-box → Contractor Co.  ⏱ 2 d left   ▸ Extend… · Revoke…
Services                ▸  jupyter on gpu-host-17 · grafana on ops-1 (click opens or copies the URL)
Agent sandboxes         ▸  asus — Regular mode · 2 sessions   ▸ Open status · Switch to YOLO… (admins)
Requests (1)            ▸  Contractor Co. wants ssh to build-box for 4 h   ▸ Approve · Deny
────────────────────────────────────────────
Exit node               ▸  None ✓ · ops-1 (Acme) · Allow local network access
────────────────────────────────────────────
Organization            ▸  Personal · Acme Robotics ✓ · Xcelsior (renter) · Add organization…
Admin console…                                       (owners and admins)
Settings…                                            ⌘,
Check for updates…
Help · Send diagnostics… · About Headmesh
Quit Headmesh                                        ⌘Q
```

**Notifications** (each one can be switched off): grant to me created, expiring in 15 minutes, revoked; access
request waiting for me; my request approved or denied; sign-in expiring in 3 days; device approval needed (admins);
update ready. Nothing for routine peer online/offline changes.

**Windows:**

- Onboarding: welcome → install the network extension (deep link to System Settings → Login Items & Extensions) →
  allow the VPN configuration → sign in → choose org → "This Mac is aaryn-mbp-acme, 100.64.0.23".
- Settings tabs: **Accounts** (orgs, sign out), **General** (launch at login, update channel), **Network** (use
  Headmesh DNS, accept subnet routes, local network access through an exit node), **Advanced** (custom server,
  logs, diagnostics, open-source licences).
- An "invite" deep link `headmesh://join?token=hmj_…` (from `app.headmesh.dev/join/<token>`) opens the app on the
  right org.

### 5.4 Robustness checklist (C2 exit criteria)

- Sleep and wake, lid close, Wi-Fi ↔ Ethernet ↔ hotspot (`NWPathMonitor`) without user action.
- Captive portals: detect them, say so in the menu, and don't fight the portal's DNS.
- Connect on boot and stay connected through VPN on-demand rules, as a user setting.
- Clean uninstall: remove the extension and VPN configuration; leave no DNS or route residue.
- **Coexistence with Tailscale on the same Mac.** Both engines claim the CGNAT range (`100.64.0.0/10`) and
  `100.100.100.100`, and Headmesh's audience often already runs Tailscale. **Spike C2-S2:** what happens when both
  run, and whether a cell can use a distinct sub-range of `100.64.0.0/10` that the Headmesh client routes narrowly.
  The xcelsior cell's hardcoded `100.64.0.x` addresses make this cell-specific. Until it is solved, detect the
  conflict and explain it in the menu rather than breaking silently.

### 5.5 Distribution

- Developer ID Application and Installer certificates, hardened runtime, notarization with `notarytool`, stapled
  tickets.
- **DMG** for people, **signed .pkg** for MDM.
- **Sparkle 2** with EdDSA-signed appcasts at `pkgs.headmesh.dev/macos/{stable,beta}/appcast.xml`, plus phased
  rollout.
- Homebrew cask in `headmesh-dev/homebrew-tap` (which the CLI already plans).
- **MDM:** managed preferences domain `dev.headmesh.macos` (`ServerURL`, `OrgHint`, `RequireSignIn`,
  `AllowedExitNodes`, `UpdateChannel`, `HideCustomServer`, `StartOnLogin`), plus sample profiles that pre-approve the
  system extension (`com.apple.system-extension-policy`) and the VPN configuration.
- **Mac App Store:** later and optional, for organizations that require store apps. It is the same code as an app
  extension.

### 5.6 Apple prerequisites (manual; start these now, they take days to weeks)

1. Enroll **Xcelsior Compute Inc.** in the Apple Developer Program as an organization ($99/year), using its existing
   D-U-N-S number. Xcelsior owns Headmesh's copyright and trademark (`LICENSING.md`), so no separate Headmesh company
   or number is needed. If Headmesh becomes its own company, switch the signing team **before the public beta
   (C3)**: a new team ID makes every user approve the network extension again, which costs nothing while only
   internal testers have the app. Everything Headmesh-specific stays in the `headmesh.dev` namespace: bundle IDs
   `dev.headmesh.*`, App Group `group.dev.headmesh`, URL scheme `headmesh://`, MDM domain `dev.headmesh.macos`,
   `pkgs.headmesh.dev`. Xcelsior then appears only as the team and seller name.
2. Enable the capabilities for the bundle IDs: Network Extensions (packet tunnel), System Extension, Keychain
   sharing, App Groups. Apple's forums report that the `-systemextension` packet-tunnel entitlement may need a
   request to Apple; check TN3134 and the account's capabilities page.
3. Create an App Store Connect API key for notarization in CI.
4. Mac CI: GitHub-hosted macOS runners (minutes cost about 10× Linux) or a self-hosted Apple-silicon Mac mini
   registered as an owner-restricted runner, like the ASUS sandbox runner.

---

## 6. Other platforms

### 6.1 Windows (C4)

- **Engine:** `headmeshd` as a Windows service on Wintun, with a named-pipe LocalAPI. This is the upstream Windows code
  path, so it is mostly packaging.
- **Tray app:** **Go**, using the same `systray` approach as upstream's open-source Linux tray (`cmd/systray`), and sharing
  code with the Linux tray. Rich views (devices, settings, sign-in) use small native dialogs plus deep links into the
  admin console. Move to WinUI 3 only if users ask for more.
- **Installer and signing:** MSI built with WiX; Authenticode signing through **Azure Trusted Signing** (Microsoft's
  managed code-signing service, about $10/month, with no hardware token to look after).
- **Updates:** a signed update manifest at `pkgs.headmesh.dev/windows/…`, checked by the service, which installs the
  MSI silently. Also publish to winget.
- **Management:** ADMX/ADML templates for Group Policy and Intune (`HKLM\SOFTWARE\Policies\Headmesh\…`, with the same
  keys as the macOS MDM domain).

### 6.2 Linux (C4)

- `headmeshd` replaces the dependency on the stock `tailscaled` from `pkgs.tailscale.com`. The existing
  instance pattern carries over (`headmeshd@<name>`), so **several networks at once** remain a Linux feature.
- Tray app: the Go tray from 6.1 (StatusNotifierItem), optional.
- Packages: deb and rpm (nfpm is already in the release pipeline), signed apt/yum repositories at
  `pkgs.headmesh.dev`, plus an AUR package. The `install.sh` planned in `plan.md` §10 (not built yet) installs
  `headmeshd` too, verifying sha256 and the cosign signature.
- Servers keep `headmesh join hmj_…` and tagged identities.

### 6.3 iOS and iPadOS (C5)

- One SwiftUI codebase with macOS (shared views and models; platform-specific chrome).
- NE app extension; the engine is slimmed with feature tags to fit the memory ceiling; memory is measured in CI on
  a device farm or on real devices before every release.
- TestFlight, then the App Store. VPN apps must come from an organization account and come under App Review rules
  for VPNs (privacy policy, clear purpose).
- Includes a share-sheet extension: "Send to a Headmesh device".

### 6.4 Android (C5)

- **Fork `tailscale-android`** (BSD-3; Kotlin, Jetpack Compose, Go engine through gomobile) under
  `headmesh-dev/headmesh-android` or `clients/android`. Rebrand completely, replace control-URL defaults and logging,
  and add Headmesh sign-in and the overview screens.
- Keep the fork rebased on upstream releases monthly. Most churn is in the Go engine, which is shared.
- Google Play: organization developer account; declare `VpnService` as the app's core function, as Play policy
  requires.

### 6.5 Headless and CI

- `headmesh login` (device flow), `headmesh up`, `headmesh status`, `headmesh ping`: one CLI that speaks to
  `headmeshd`.
- **GitHub Action** `headmesh-dev/connect` (C6): joins an ephemeral, tagged CI runner for one job with a short-lived
  token. It costs little to build and is valuable.

---

## 7. Feature parity matrix

Where each feature lives: **H** = Headscale already supports it (Headmesh only compiles policy for it);
**C** = client-side only; **M** = Headmesh builds it (API, server or console); **U** = needs upstream Headscale work.
Headscale's support is from its features page and the v0.29 changelog; entries marked *verify* need a spike.

| Feature | Headscale v0.29.7 | Headmesh today | Plan | Lives in | Phase |
|---|---|---|---|---|---|
| Desktop and mobile apps | — | none | §5, §6 | C | C2–C5 |
| SSO sign-in | OIDC supported (Headmesh bypasses it) | GitHub, HF | + Google, Microsoft, passkeys, email link; enterprise OIDC; SAML and SCIM later | M | C1, C6 |
| MagicDNS | ✓ | names `<name>-<orgslug>.mesh.headmesh.dev` | client applies DNS | H + C | C2 |
| Split DNS, custom nameservers | ✓, but **per cell** (global) | — | per-org DNS through the product plane, applied locally by Headmesh's own client | M + C | C6 |
| Subnet routers | ✓ | off by default; approval within org prefixes | accept routes in the client. **Risk:** overlapping RFC 1918 prefixes from two tenants; Headscale's primary-route choice may be cell-wide (*verify*) | H + M | C3 |
| **Exit nodes** | ✓, plus Via | **blocked by the isolation rule** | §7.2 | H + M | C3 |
| Tailscale-protocol SSH | ✓ (policy `ssh`) | compiler emits `ssh` for opted-in orgs | SSH server in `headmeshd` (Linux, macOS daemon); "SSH" menu item | H + C | C3 |
| File sharing between own devices (Taildrop) | ✓ (same user) | ✗ (tagged nodes) | works once personal devices are user-owned (§4.6) | H | C3 |
| File sharing across users or orgs | grant `app` capabilities (*verify*) | — | grant capability, if Headscale passes it through | H + M | C6 |
| Taildrive | ✓ (`cap/drive`) | — | compiler emits drive capabilities per org | H + M | C6 |
| **Peer relays** | ✓ (`cap/relay`) | — | let orgs run their own relays instead of DERP; compiler emits relay capabilities for org-owned relay nodes only | H + M | C6 |
| DERP relays | embedded server | region 900 on hm-prod-1 | **own regions (EU, NA, APAC) before GA** (§9.4) | M | C6 (before GA) |
| Key expiry and re-auth | ✓ for user nodes | n/a (all tagged) | org setting; background re-auth (§4.5) | H + C + M | C1–C2 |
| Device approval | — | `approval_required` on join tokens | for personal devices too; approve from the menu | M | C3 |
| Ephemeral nodes | ✓ | supported in tokens | CI action | H | C6 |
| Several accounts (fast switching) | — | — | one engine profile per org | C | C3 |
| **Several networks at once** | — | ✓ on Linux (instances) | research for desktop | C | C6 |
| Shares with expiry, access requests | — | grants built | menu countdown; request and approve flow | M + C | C3 |
| Serve (local HTTP proxy) | client feature (Headscale lists it with an open issue) | — | identity headers need user-owned devices | C | C6 |
| Public ingress (Funnel equivalent) | ✗ (open issue) | ingress planned for MCP (`plan.md` §7.5) | **"Public links"**: generalize the ingress to any HTTP service | M | C6 |
| HTTPS certificates | not listed | — | ACME DNS-01 on `mesh.headmesh.dev` via Cloudflare; the device keeps its private key. Note: names appear in Certificate Transparency logs | M + C | C6 |
| Device posture | not listed | — | client reports OS version, disk encryption, firewall, MDM state; the compiler moves non-compliant devices into quarantine. No Headscale support needed | M + C | C6 |
| Network flow logs | ✗ (open issue) | — | the engine's flow logger, enabled by Headmesh's own client and sent to a Headmesh endpoint (*verify*) | C + M | C6+ |
| Config audit log | — | ✓ hash chain | console viewer | M | C3 |
| Webhooks | — | queue schema | finish the API (§9.10) | M | C3 |
| API, OAuth clients | gRPC (not exposed) | `/v1`, API keys | SDKs; Terraform provider | M | C6 |
| MDM and Group Policy | — | — | §5.5, §6.1 | C | C3–C4 |
| Auto-update | — | — | Sparkle, MSI updater, repositories, stores | C | C2–C5 |
| Admin console | — | — | §8 | M | C3 |
| Tailnet Lock | ✗ | — | research; would need Headscale to support it. Strong trust story for a hosted multi-tenant service | U | post-GA |
| App connectors | not listed | — | later | U | post-GA |
| Kubernetes operator | — | — | later; Headmesh's API instead of Tailscale's | M | post-GA |
| Commercial VPN exit nodes | — | — | **not planned** | — | — |

### 7.1 Picking the 20%

Parity is endless. The features that matter for **real** users first are sign-in, connect, MagicDNS, the device list,
shares with countdowns, SSH, exit nodes, file sharing between your own devices, updates and the console. Everything in
C6 is earned by usage data.

### 7.2 Exit nodes need an isolation design, not a toggle

`AGENTS.md` forbids emitting `autogroup:internet` from tenant data, because a broad internet grant could make
another tenant's exit node visible or usable. Exit nodes still work in a multi-tenant cell if:

- the compiler emits `autogroup:internet` **only** together with `via: [tag:hm-n-<device>]`, where every named device
  is an approved exit node **of the same org**;
- new invariants (I8: no node may route through another org's exit node; I9: exit-node visibility stays inside the
  org) get property tests, and `test/e2e` proves no cross-tenant visibility or routing;
- the org setting `allow_exit_nodes` stays off by default, and approving an exit node is an audited admin action.

This changes a written rule, so it needs **Aaryn's explicit sign-off** and an ADR before any code.

---

## 8. Admin console (web/app)

Tailscale calibre is half app, half console. `plan.md` §12.2 already chooses the stack (Vite, React, TanStack,
shadcn/ui, a generated OpenAPI client, embedded into `headmesh-server`). Order of pages:

| Phase | Pages |
|---|---|
| C1 | Sign-in (existing providers + Google/Microsoft/passkey), create org, accept invite, **"Download the app"** with OS detection |
| C3 | **Machines** (list, rename, remove, approve, key expiry, tags/roles), **Users** (members, roles, invites), **Sharing** (grants in and out, requests), **Access** (rules, preview, access check explain), **Audit log**, **Settings** (org, API keys, exit nodes, SSH, file sharing) |
| C6 | DNS, webhooks, SSO (enterprise OIDC), posture policies, **Sandbox MCPs** (1-click MCP Registry catalog, `mesh-only`/`wan-only` profile & secret binding per `sandbox/docs/adr/0002-*`), billing, cell and usage views for platform orgs |

The console and the apps use the same API. No feature exists only in the console.

---

## 9. Downloads, updates and release engineering

### 9.1 Download experience

- `headmesh.dev/download`: OS and architecture detection, with one primary button per OS and links to the others.
  It lives on the marketing site (Astro), so it ships independently of the product.
- `app.headmesh.dev/join/<token>` (already planned as OS-detecting) tries `headmesh://join?token=…` first and falls
  back to the download.
- **Downloads are public; access is not.** Every install is useless until the user signs in, which is the same model
  as Tailscale and the simplest to support. "Authenticated" means sign-in in the app, device approval where the org
  requires it, and an audit entry for every enrollment.

### 9.2 Hosting

`pkgs.headmesh.dev` on Cloudflare R2 behind Cloudflare: macOS DMG, pkg and appcasts; Windows MSI and manifests; apt
and yum repositories; checksums and cosign signatures. A small CI job generates appcasts and manifests and
uploads them. It is cheap, and it reuses accounts that already exist.

### 9.3 Signing matrix

| Platform | Signing | Where the secret lives |
|---|---|---|
| macOS | Developer ID + notarization; Sparkle EdDSA key for appcasts | GitHub environment secrets on a protected `release` environment |
| Windows | Authenticode through Azure Trusted Signing | Azure, through GitHub OIDC (no exported key) |
| Linux | GPG repository key; cosign keyless on artifacts (already used) | Offline primary key, CI subkey |
| iOS | App Store distribution | App Store Connect |
| Android | Play App Signing | Google Play |

### 9.4 DERP

The client follows the cell's DERP map. Before public launch, Headmesh should run its own regions (at least EU and
North America, ideally APAC) instead of relying on Tailscale's public relays for a competing product (`plan.md` §19.6
already flags the second location). Peer relays (§7) can cut DERP load later.

### 9.5 Release trains and compatibility

- Client versions are semver and independent of the server: `headmesh-client 1.x`.
- Channels: **nightly** (internal), **beta** (TestFlight, Sparkle beta, winget preview), **stable**, with phased
  rollouts (Sparkle phased rollout; percentage gates in the Windows manifest).
- `/v1/client/config` returns `min_client_version`. Below it, the app shows "Update required" and keeps working
  where it safely can.
- **CI matrix:** every client build × every Headscale version the cells run (today v0.29.6/7) × `test/e2e`. A
  weekly job bumps `tailscale.com` to the latest release and opens a PR when green.

---

## 10. Client security and privacy

| Threat | Control |
|---|---|
| Stolen laptop | Key expiry for personal devices; remote remove from the console; tokens in the OS keystore; optional "require sign-in every N days" |
| Token theft | 15-minute access tokens; rotating refresh tokens with reuse detection (already in the OAuth design); DPoP-bound tokens later |
| Malicious update | Code signing on every OS; Sparkle EdDSA appcasts; the updater refuses downgrades; signed manifests on Windows |
| Rogue enrollment | Single-use 5-minute pre-auth keys minted only by the cell agent; device approval for orgs that want it; quarantine of unknown nodes (existing) |
| Phishing through a "custom server" | Clear warning UI; MDM can hide or pin the server |
| Data leaking through logs | **No telemetry by default** (already the CLI's policy in `plan.md` §10). Logs stay local; "Send diagnostics" is an explicit, redacted upload to Headmesh. The engine's own log upload is compiled out or redirected (§3.2) |
| Contacting Tailscale's servers | CI egress-allowlist test (§3.2) |
| The client as an enforcement point | Never. Headscale enforces the compiled policy; posture and other client reports are inputs to the compiler, never the last line of defence |

Before GA: an external pen test of the clients and the OAuth flows, a privacy policy and App Store privacy labels,
`SECURITY.md` coverage for the clients, and a crash reporter that is off by default (Sentry, with the user's consent).

---

## 11. Phased roadmap

### C0 — Foundations (2–3 weeks)

- Write ADRs 0016–0020 (§15) and get them accepted.
- Create the `clients/` Go module and `headmeshd` on `tailscale.com` v1.104.x with Headmesh defaults: no Tailscale
  control, log or update endpoints.
- Add the egress-allowlist CI test and the client × Headscale matrix; move CI's `tailscale` image to the latest
  v1.104.
- Upgrade the cells to Headscale **v0.29.7**. This touches production, so announce it first and get Aaryn's go-ahead.
- Manual: Apple Developer organization enrollment (D-U-N-S), Azure Trusted Signing, Google Play organization account.
- Brand assets for clients: menu bar template icons, app icons, tray icons.

**Exit:** `headmeshd` plus the CLI join hs1 on Linux with no connections to Tailscale-owned hosts; `test/e2e`
passes with `headmeshd` in place of `tailscaled`.

### C1 — Identity and product plane (3–5 weeks; overlaps M3)

- Orgs, members and invites in the API (`plan.md` §9.8), with user-scoped authorization beside API keys.
- First-party OAuth clients: `headmesh-desktop` (PKCE, `headmesh://` and loopback redirects), `headmesh-cli`
  (device flow, RFC 8628).
- Person-owned devices (§4.6): ADR, migration, compiler with property tests, cell-agent `EnsureUser`, e2e.
- `/v1/devices/register`, `/v1/client/config`, `/v1/client/overview`, `/v1/events/stream`.
- CLI: `headmesh login`, `up`, `down`, `status`, `switch`.
- Console: sign-in, create org, invite, download page.

**Exit:** on a fresh Linux laptop, `headmesh login && headmesh up` enrolls a user-owned device in the user's org; the
isolation e2e is green with mixed tagged and user-owned nodes; the revocation SLO is unchanged.

### C2 — macOS alpha (4–6 weeks)

- Spike C2-S1 (the NE bridge) and C2-S2 (coexistence with Tailscale) in week 1.
- Menu bar app: sign-in, connect and disconnect, this device, my and org devices with copy actions, shared-with-me
  with countdowns, basic notifications, settings, onboarding.
- Developer ID, notarized DMG, Sparkle beta channel.
- Dogfood on Aaryn's devices and with Xcelsior's team (renters reaching rented GPUs through grants is the real test).

**Exit:** a fresh Mac goes from download to connected in under 2 minutes; a 7-day soak across sleep and network
changes with no manual reconnects; zero traffic to Tailscale-owned hosts; crash-free sessions above 99%.

### C3 — macOS beta, console v1, downloads (4–6 weeks)

- Exit nodes (after the §7.2 ADR and sign-off), accept subnet routes, MagicDNS polish, file sharing between own
  devices, SSH action, services menu, access requests and approvals, several orgs, device approval.
- MDM domain and sample profiles; signed pkg; Homebrew cask; `headmesh.dev/download`.
- Console v1 pages (§8); webhooks API finished.

**Exit:** 20–50 external beta users; the funnel from sign-up to connected device is measured; a support runbook
exists.

### C4 — Windows and Linux (6–8 weeks; can start after C1)

- Windows service, Go tray, MSI, Azure Trusted Signing, updater, winget, ADMX.
- Linux `headmeshd` packages and repositories, tray, AUR.

**Exit:** the same funnel and soak criteria as C2 on Windows 11 and on Ubuntu 26.04 plus one rpm distribution.

### C5 — Mobile (8–12 weeks)

- iOS: shared SwiftUI, NE app extension, memory budget, TestFlight, App Store review.
- Android: fork, rebrand, Headmesh screens, Play internal testing, then production.

**Exit:** both store listings live; mobile e2e smoke tests (join, reach a peer, receive a share) in CI.

### C6 — GA and differentiation (ongoing)

In rough order of value: own DERP regions (before GA) → GitHub Action → HTTPS certificates → public links → device
posture → peer relays → enterprise OIDC, then SAML and SCIM → Terraform provider → per-org DNS → Mac App Store
build → flow logs → Tailnet Lock research → several simultaneous networks on desktop.

### Timeline sketch

```
Weeks         0    4    8    12   16   20   24   28   32   36   40   44   48
C0 Found.     ███
C1 Identity     █████
C2 macOS α          ██████
C3 macOS β                ██████
C4 Win/Linux          ████████████            (parallel once C1 lands; needs a second pair of hands to overlap)
C5 Mobile                         ████████████████
C6 GA+          DERP before GA ─────────────────────────────────────────────►
Core M3–M5   ████████████   (sign-up, console, hardening; C1 shares this work)
```

---

## 12. Team, accounts and costs

| Item | Cost | Notes |
|---|---|---|
| Apple Developer Program (organization) | $99/year | D-U-N-S number first; covers macOS and iOS |
| Google Play Console | $25 once | Organization verification |
| Azure Trusted Signing | about $10/month | Windows Authenticode |
| DERP regions (3 small VMs) | about €6–20/month each | Hetzner EU is cheap; US and APAC cost more (`plan.md` §1) |
| macOS CI | GitHub macOS minutes, or about $600 once for a Mac mini runner | The Mac mini also serves as the soak-test machine |
| `pkgs.headmesh.dev` on R2 | about $0 at first | R2 has no egress fees |
| Crash reporting | free tier | Opt-in only |
| External client pen test | $5–15k | Before GA |

**Skills:** Go (in place). Swift, SwiftUI and NetworkExtension are the hardest and rarest; a short contract with an
engineer who has shipped a Network Extension VPN makes C2 much less risky. Kotlin is needed for the Android fork,
and Windows packaging for C4.

---

## 13. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `tailscale.com` internals change between releases (they are not a stable public API) | High | Medium | Thin wrapper; pinned tags; weekly bump PR with the full e2e; few touch points |
| Headscale's capability window drops the client's version, or a new client needs a newer Headscale | Medium | High | CI matrix; upgrade cells before shipping clients; `min_client_version` gate |
| Apple entitlements, notarization or review delays | Medium | High | Start accounts in C0; daemon fallback for the alpha; App Store build is optional for macOS |
| Network Extension complexity (sleep, DNS, routes, iOS memory) | High | High | Time-boxed spike; wireguard-apple as reference; specialist contractor; soak tests |
| Clash with an installed Tailscale (CGNAT range, `100.100.100.100`) | High for this audience | Medium | Spike C2-S2; distinct sub-range per cell if feasible; clear in-app explanation |
| Multi-tenant hazards in exit nodes and overlapping subnet routes | Medium | **Critical** (isolation) | §7.2 design, new invariants, e2e, owner sign-off; subnet routes stay off by default |
| Trademark or look-alike complaints | Low | Medium | Own name, icons and UI; factual attribution only |
| Depending on Tailscale's DERP fleet | Medium | Medium | Own regions before GA |
| Support load from consumer users | High | Medium | Diagnostics bundle, status page, docs per error state in the app |
| Scope creep toward full parity | High | High | §7.1; C6 driven by usage data |

---

## 14. Decisions needed from Aaryn

| # | Decision | Recommendation |
|---|---|---|
| 1 | Client strategy | **Option C**: own apps on the upstream `tailscale.com` library; official Tailscale apps stay supported as "bring your own client" |
| 2 | Identity for personal devices | **User-owned nodes, one Headscale user per (person, org)**; infrastructure stays tagged (amends ADR 0011) |
| 3 | Exit nodes | **Allow** `autogroup:internet` only with `via` naming the same org's exit nodes, behind new invariants and e2e (changes the `AGENTS.md` rule) |
| 4 | First platform and shape | **macOS 14+, Developer ID, Network Extension as a system extension**, with a daemon fallback |
| 5 | Windows and Linux GUI technology | **Go tray app** shared by both; native WinUI only if needed |
| 6 | Android | **Fork `tailscale-android`** |
| 7 | Downloads | **Public**; sign-in in the app; invite links open the app |
| 8 | DERP | **Own regions before GA** |
| 9 | Licence and layout | **Apache-2.0** clients in a separate `clients/` Go module; `NOTICE` for BSD-3 code |
| 10 | Accounts | **Start Apple (D-U-N-S) and Google enrollment now**; they gate C2 and C5 |
| 11 | One CLI or two | **One `headmesh` CLI**, eventually moved into `clients/` with engine subcommands (`status`, `ping`, `netcheck`, `ssh`) |

---

## 15. ADRs to write

| ADR | Title |
|---|---|
| 0016 | Headmesh's own clients on the upstream `tailscale.com` library (no fork, no phoning home) |
| 0017 | Person-owned devices: one Headscale user per (person, org) (amends 0011) |
| 0018 | Client architecture: two planes, LocalAPI extensions, the `clients/` module |
| 0019 | macOS: Network Extension as a system extension, Developer ID, Sparkle |
| 0020 | Native-app OAuth: first-party public clients, PKCE, custom scheme and loopback, device flow |
| 0021 | Exit nodes in multi-tenant cells (`via`-scoped `autogroup:internet`) |
| 0022 | Windows and Linux tray technology |
| 0023 | Android: fork of `tailscale-android` |
| 0024 | Release engineering: `pkgs.headmesh.dev`, signing, channels, compatibility gates |
| 0025 | Own DERP regions |

---

## 16. First two weeks

1. Decide §14, items 1–4 and 10. Start the Apple and Google account enrollments the same day.
2. Plan the Headscale v0.29.7 upgrade for both cells (announce it; production needs a go-ahead).
3. Write ADR 0016 and scaffold `clients/` with `cmd/headmeshd` building against `tailscale.com` v1.104.7.
4. Add the egress-allowlist test, then run `test/e2e` with `headmeshd` in place of `tailscaled`.
5. Write ADR 0017 and the compiler design for user principals; extend the rapid generators first, before any code.
6. Draft the product-plane OpenAPI (§4.8) so the console and the macOS app can be built against mocks.
7. With the Apple account ready, run spike C2-S1 on a test Mac against hs1.

---

## Appendix A — Proposed layout

```
clients/                          # Go module github.com/headmesh-dev/headmesh/clients (Apache-2.0)
  go.mod                          # pins tailscale.com
  engine/                         # wrapper: defaults, feature tags, no-phone-home, tunnel-settings callbacks
  sdk/                            # Headmesh API client (generated from api/openapi), OAuth, discovery, SSE
  overview/                       # netmap + product-plane merge
  localapi/                       # /localapi/v0/headmesh/* handlers
  cmd/headmeshd/                  # daemon: Linux service, Windows service, macOS launchd (dev and servers)
  cmd/headmesh-tray/              # Windows and Linux tray
  bind/                           # c-archive (Apple) and gomobile (Android) entry points
  apple/                          # Xcode project: macOS app + system extension, iOS app + app extension, shared SwiftUI
  android/                        # tailscale-android fork (or a separate repository)
  windows/                        # WiX project, ADMX/ADML
  packaging/                      # appcast and manifest generators, notarize and sign scripts, nfpm configs
  tests/                          # egress allowlist, Headscale matrix, soak scripts
```

The AGPL boundary check that `LICENSING.md` suggests ("a test in CI could enforce this later") should become real in C0
and cover `clients/` as well as `cmd/headmesh` and `sandbox/`.

## Appendix B — Upstream LocalAPI the GUIs use

`status` (peers, self, health), `prefs` and `prefs` patch (want-running, exit node, accept routes, DNS), `login-interactive`
(not used: Headmesh signs in through its own API), `start` with an auth key, `logout`, `profiles` (list, switch,
add), `watch-ipn-bus` (live state), `suggest-exit-node`, `file-targets` and `file-put` (file sharing), `ping`,
`whois`, `cert` (replaced by the Headmesh flow in C6), `bugreport` (feeds "Send diagnostics").

## Appendix C — Sources checked for this document (2026-11-14)

- Headscale features page: <https://headscale.net/stable/about/features/> (peer relays, Taildrive, Via, Taildrop
  listed as supported; Funnel, Serve and flow logs listed against open issues).
- Headscale releases: v0.29.7 (2026-10-30), v0.29.6 (2026-10-24), from the GitHub API; v0.29 changelog notes on
  `cap/relay` and `cap/drive` grants.
- Tailscale's macOS variants: <https://tailscale.com/docs/concepts/macos-variants> and the "Tailscaled on macOS" wiki
  (open-source daemon: utun, runs before login, can't use exit nodes, Taildrop incomplete).
- Tailscale's custom control server guide: <https://tailscale.com/docs/how-to/set-up-custom-control-server> (the
  Windows GUI can't set one; registry or CLI only).
- Licences from the GitHub API: `tailscale/tailscale` BSD-3-Clause, `tailscale/tailscale-android` BSD-3-Clause,
  `WireGuard/wireguard-apple` MIT; latest Tailscale release v1.104.7 (2026-11-12).
- Upstream code at v1.104.7: `ipn.DefaultControlURL`, `TS_LOG_TARGET` and the logtail feature gate in
  `logpolicy`; `cmd/systray`.
- Apple Developer Forums threads on Developer ID distribution of packet tunnel providers as system extensions
  (TN3134).
