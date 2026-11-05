# Directory submission drafts

The marketplace installs and public directory submissions use different MCP
configuration contracts. The installed portable manifests retain the tested
Codex-compatible shape. OpenAI's upload draft adds explicit discovered CIMD OAuth
under each server's `extensions.com.openai.auth`, as required by the
[OpenAI package guide](https://developers.openai.com/plugins/build/plugins).

Build both draft ZIPs from the existing plugin manifests, tracked skills and
canonical assets:

```sh
pnpm --dir plugins check
pnpm --dir plugins package:submissions --out /absolute/new/output-directory
```

The destination must be new. Existing artifacts are preserved. The receipt
records ZIP hashes, component hashes and remaining review work. The builder
copies only tracked assets and skills, preserves existing prompts and skill
bytes, rejects symlinks and embedded server credentials, validates the base
manifests, and checks ZIP integrity. It does not call models or submit anything.

`plugins/submission.source.json` supplies draft subtitles, support links and five
positive plus three negative review cases per MCP server. Existing starter
prompts come unchanged from the plugin manifests. Commerce is described
explicitly; the drafts do not claim policy acceptance. Review cases still need
to be exercised in the actual provider clients.

## OpenAI

Portal: https://platform.openai.com/plugins

Upload the relevant `*-openai-draft.zip`, then follow the
[submission flow](https://developers.openai.com/plugins/deploy/submission).
Publisher verification, domain ownership, a usable reviewer account, a hosted
walkthrough and the portal's connection/tool checks remain external steps.
Reviewer credentials belong only in the secure portal form, never in the ZIP.

For a self-hosted service with a different endpoint for each customer, check
the [template URL requirements](https://developers.openai.com/plugins/deploy/app-review#template-mcp-server-urls).
A fixed endpoint for one owner's sandbox is a custom connection; it is not a
universal public service. The package does not grant access to shared machines.
The full GPU/payment tool surface also needs review against the
[commerce rules](https://developers.openai.com/plugins/plugin-guidelines#commerce-and-monetization).
Packaging does not remove tools or change their behavior to imply eligibility.

## Claude

Portal: https://claude.ai/directory/manage/new

Submit each owned remote MCP server as an **MCP connector**. Submit a plugin
bundle separately if you also want its skills listed, using the corresponding
folder in this public repository:

- `plugins/xcelsior`
- `plugins/headmesh`

Use the same publishing organization for a connector and its plugin bundle.
Follow the [directory publication guide](https://claude.com/docs/directory/publish)
and [connector review criteria](https://claude.com/docs/connectors/building/review-criteria).
The portal needs an eligible account, reviewer access and real client testing.
Its policy restrictions must be evaluated against the actual full tool surface.

## Registry and Hugging Face

The official MCP Registry uses `sandbox/server.json` for Headmesh and the
Xcelsior repository's `api/mcp/server.json`. Registry publication is distinct
from either provider's curated directory.

The Hugging Face documentation Space is maintained under
`integrations/huggingface/`. A Hub publishing token is separate from the
Hugging Face OAuth app's client secret. Keep the publishing token in the private
operator `.env` as `HF_TOKEN`; the backend does not need it.
