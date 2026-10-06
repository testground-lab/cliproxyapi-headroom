# Headroom for CLIProxyAPI

Native CLIProxyAPI plugin that compresses eligible user-message and tool-result text through Headroom and adds a standalone statistics page served by CLIProxyAPI. No manager modifications are required.

## Features

- OpenAI Chat, Responses/Codex, Anthropic and Gemini user-message and tool-output compression.
- Preserves system messages, tool IDs, images and provider routing.
- Marker-free Headroom compression with original-request fallback on failure.
- Persistent CPA-only savings, latency, model breakdowns and 90-day hourly history.
- Separate Headroom service view reading `/livez`, `/readyz`, `/health`, `/stats` and `/stats-history`.
- Authenticated statistics APIs, live refresh, responsive charts and JSON export.

## Request flow

```text
Client → CLIProxyAPI → Headroom /v1/compress → CLIProxyAPI → selected provider
```

Headroom returns compressed content; it does not forward the generation request. CLIProxyAPI retains authentication, OAuth, routing, translation and streaming.

## Requirements

Tested with CLIProxyAPI v7.3.17 and Headroom v0.37.0 on Linux amd64. Release archives target Linux amd64/arm64, macOS amd64/arm64, and Windows amd64; native platform CI validates each build. Headroom must run separately and support `/v1/compress` with an omitted `config.mode` (server defaults) or `config.mode: lossy_inline`. The earlier compression-only plugin was also exercised with the existing deployment originally labelled 0.33.0; the live service now reports 0.37.0.

## Install

Download `headroom_0.6.0_linux_amd64.zip` and `checksums.txt` from [Releases](https://github.com/frankyw/cliproxyapi-headroom/releases). Verify the checksum, extract `headroom.so`, and install it as `plugins/linux/amd64/headroom-v0.6.0.so` in CLIProxyAPI's persistent plugin directory. Back up your config and retain other plugin settings when merging:

```yaml
plugins:
  enabled: true
  configs:
    headroom:
      enabled: true
      priority: 10
      endpoint: http://headroom:8787/v1/compress
      stats_path: plugins/data/headroom/stats.json
      timeout_ms: 10000
      min_chars: 512
      target_ratio: 0.5
      mode: ""
      compress_user_messages: true
      token_env: ""
      service_url: ""
```

Restart CLIProxyAPI, then refresh the manager and select **Headroom Stats**. The page stays in the manager menu iframe. You can also open `http://<CPA-host>:8317/v0/resource/plugins/headroom/stats` directly. For saved-key reuse, open it on the same host and port as the manager where you signed in. Both Docker containers must share a network. Headroom requires `HEADROOM_COMPRESS_ALLOW_REMOTE=1` for compression requests from another container. If authentication is configured, set `token_env` to the name of a Headroom-token environment variable available to CLIProxyAPI; client and provider credentials are never forwarded.

`mode: ""` (the default) follows the Headroom server's mode; recommended with `headroom proxy --lossless`. Set `mode: "lossy_inline"` for the previous behavior, which can drop words from text. Other mode values are rejected.

`service_url` optionally overrides the service root used for health/statistics. Empty uses the origin of `endpoint`. Use it if Headroom is hosted under a URL prefix. Keep these operator-configured endpoints on trusted infrastructure.

## Dashboard access

The **Headroom Stats** menu embeds `/v0/resource/plugins/headroom/stats` in the manager. The page reads the manager credential from same-origin `cli-proxy-auth`, supporting both `enc::v1::` and `enc::v2::` storage. In CPA Manager Plus server mode, select **Remember credential** at login so its Admin Key is saved on the CPAMP origin; in a direct CPA Manager login, select **Remember password** for the CPA Management Key. The page sends the saved value only to management routes on its own origin. Browser storage is isolated by host and port, so open the page from the same manager origin where you signed in. If a login was not saved, the page offers a per-tab credential field. No manager changes are required.

Only the static HTML is available under `/v0/resource/plugins/headroom/stats`. The former public `/stats-data` and `/service-data` resource routes return 404. Statistics and filtered Headroom service health require CPA's management authentication at `/v0/management/plugins/headroom/stats` and `/v0/management/plugins/headroom/service-stats`. The page sends the key only to these same-origin CPA routes. It never includes prompts, tool output, request headers, API keys, or Headroom project labels in its data.

## Statistics and interpretation

CPA-only counters begin when v0.2+ is enabled. They record eligible-text reductions accepted by this plugin before provider execution, not successful/billed provider requests. Before v0.5.0, only tool results were eligible. Token estimates are recorded only when Headroom's returned text is fully accepted and its token counters are valid. Partially accepted reductions retain byte metrics without claiming token savings. Requests without eligible text, unchanged responses and fallback failures are counted separately.

Snapshots are written atomically every two seconds and on orderly plugin shutdown. Abrupt termination may lose the last two seconds. `stats_path` must reside on persistent storage; empty means memory-only. The file stores aggregate counts, bounded model labels, 90 days of hourly history and the latest 100 metadata-only events. No prompts, tool contents, request headers or API keys are stored. Lifetime totals remain after hourly retention expires. A corrupt file is not silently overwritten; configuration fails so the operator can restore it.

The service section separately reads all five Headroom endpoints in parallel, with a five-second deadline and ten-second cache. It displays health, aggregate request/token statistics, display-session/lifetime savings and durable history. These totals include other Headroom clients and older traffic; they are never added to the CPA-only counters. Endpoint details are filtered to omit configuration, internal URLs and project labels. They are not a verbatim dump of every field. The service view includes up to 720 hourly and 90 daily buckets, with its chart showing the latest 30 daily buckets.

## Compression scope and limitations

Eligible text includes Chat `tool`/legacy `function` content, Anthropic `tool_result` text, Responses `function_call_output`, and string leaves within Gemini `functionResponse.response` objects. With `compress_user_messages: true` (the default), it also includes user-role string content and text blocks in Chat, Anthropic, Responses and Gemini requests. System messages remain untouched. Text shorter than `min_chars` bytes passes through. Gemini numeric fields and strings directly inside arrays are not compressed.

The setting is read from CLIProxyAPI's plugin config at registration/reconfiguration and passed to Headroom in every compression request. Set it to `false` to restore tool-result-only behavior. Headroom's `HEADROOM_SAVINGS_PROFILE` controls server-side defaults and transforms; it cannot make this plugin select a field it has skipped. The per-request `compress_user_messages` value from this plugin takes precedence over that default. When the selected Headroom mode is lossy, user-message compression can shorten both documents and instructions within the same message. Test answer quality for your workload before relying on exact quotations or extraction.

The plugin defaults to Headroom's server-configured marker-free pipeline and rejects CCR hashes. It does not provide retrieval tools, contextual cross-message optimization or provider prefix-cache tracking. Compression may be lossy depending on the selected mode; savings and answer quality depend on workload. The target ratio is not guaranteed. WebSocket behavior depends on the host invoking its interception hook and has not been independently tested.

Compression errors/timeouts preserve the original request. Bodies over 32 MiB bypass compression. Pending compression calls may continue until their deadline after a client disconnects because the native RPC interface does not expose the host request context. Metrics are not a billing ledger.

## Build and verify

Requires Docker and Python 3:

```sh
sh scripts/build.sh
HEADROOM_TEST_URL=http://headroom:8787/v1/compress sh scripts/test-live.sh
```

The build uses Go 1.26 with a C compiler and `-buildmode=c-shared`, not Go's compiler-specific plugin format. Override release metadata with `REPOSITORY_URL` and `PLUGIN_AUTHOR`; GitHub Actions populates them automatically.

On Linux with Docker, Python 3 and PyYAML, `python3 tests/integration.py` runs an isolated CLIProxyAPI container and mock provider with Headroom at `127.0.0.1:8787`, ports 18318/18319, and no production credentials. It verifies compression, streaming, rejected public data routes and authenticated management APIs, standalone page registration, all five service endpoints and restart persistence. Work files remain in ignored `work/`.

## Releases and plugin store

Push a tag matching `plugin.go`, currently `v0.6.0`. GitHub Actions runs race tests and builds native C-shared libraries on five hosted runners. The release contains `headroom_0.6.0_<goos>_<goarch>.zip` for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64, plus `checksums.txt`. Each archive contains only `headroom.so`, `headroom.dylib`, or `headroom.dll` at its root.

See `store/registry-entry.json` and `store/PR.md` for the prepared official store entry. Store inclusion requires an upstream PR; publishing this repository does not itself list the plugin.

## Disable / rollback

Set `plugins.configs.headroom.enabled: false` and restart CPA. For a version rollback, stop CPA, remove the newer versioned library, restore the previous library/config, then start CPA. Never overwrite a loaded native library. Retain the statistics file if you want to preserve history.

MIT licensed. C ABI glue is adapted from CLIProxyAPI's MIT example; the original notice is retained in LICENSE.
