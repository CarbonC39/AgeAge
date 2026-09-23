# Completed Stage: Security Baseline and Tool Capability Model

## Objective

Establish a production-ready security baseline for the public API, network access, IM interactions, sensitive local data, and tool scheduling without changing the basic CLI, IM, HTTP, MCP, or pipeline workflows.

This stage is complete when:

- The HTTP API supports optional authentication and enforces request-size, concurrency, and CORS policies.
- Web and browser access block local, private, link-local, and cloud-metadata targets by default and respond to cancellation.
- IM confirmation and `ask_user` responses are bound to the correct session, thread, and initiating user.
- The credential file is always protected, and memory persistence uses private permissions and concurrency-safe updates.
- Tool read-only, idempotency, risk, timeout, and network properties are expressed through metadata rather than hard-coded tool names.

## General Constraints

- Preserve backward-compatible defaults. The HTTP server must continue to bind to `127.0.0.1` by default, and local development must remain possible without an API key.
- Document every new configuration field in `example.config.toml`, the init template, and `docs/config.md`.
- Do not introduce a new always-on external service dependency.
- Security decisions must fail closed, and errors must never expose credential values.
- Production code must not replace a caller-provided cancellation context with `context.Background()`.
- Do not modify unrelated or pre-existing untracked files.
- Every node must include unit tests. Final validation must run `gofmt`, `go vet ./...`, and `go test -race -count=1 ./...`. Environments without a C compiler must at least run `CGO_ENABLED=0 go test ./...`.

## Node A: HTTP API Security Boundary

Owned areas: `server/`, the server section of `config/`, relevant portions of `main.go`, and configuration documentation.

Tasks:

1. Add optional Bearer API-key authentication to `/v1/*`; make `/health` authentication explicitly configurable.
2. Add a request-body byte limit and return `413` when exceeded.
3. Add an in-process concurrency limit and return `429` when saturated. Streaming requests must retain their slot for the entire run.
4. Replace fixed wildcard CORS with configuration. Preserve permissive behavior when unconfigured and unauthenticated, but never emit an invalid wildcard/credential combination.
5. Compare API keys in constant time.
6. Add tests for authentication, CORS, body limits, and concurrency-slot release.

Acceptance criteria:

- Missing, incorrect, and correct API keys are all covered by tests.
- `/health` authentication behavior is controlled by an explicit setting.
- Errors retain the existing OpenAI-style JSON shape.
- Concurrency slots are released after success, failure, and client cancellation.

## Node B: Network Egress Security and Cancellation

Owned areas: `tools/web.go`, `tools/browser.go`, relevant configuration, and tool documentation.

Tasks:

1. Implement a reusable URL/network target validator that rejects loopback, private, link-local, unspecified, multicast, and common cloud-metadata targets by default.
2. Allow only `http` and `https`, reject URLs containing user information, and validate every IP returned by DNS.
3. Revalidate every native HTTP redirect target to prevent redirect and DNS-rebinding bypasses.
4. Reuse the same policy for `web_fetch`, search follow-up requests, and browser navigation. Provide explicit allowlists or an allow-private option for trusted environments.
5. Use `NewRequestWithContext` for HTTP calls and propagate the tool-call `context.Context` through browser backends instead of creating `context.Background()` contexts.
6. Add coverage for IPv4, IPv6, localhost, private ranges, redirects, explicit allowlists, and cancellation.

Acceptance criteria:

- `127.0.0.1`, `::1`, RFC1918, link-local, and metadata endpoints are inaccessible by default.
- Public URL behavior remains unchanged.
- `/stop` or context cancellation terminates web and browser calls.
- A public URL cannot redirect into a restricted address.

## Node C: IM Authorization Binding and Sensitive Data

Owned areas: `tools/confirmation.go`, `tools/user_input.go`, `tools/memory.go`, `agent/factory.go`, the relevant IM handling in `main.go`, and channel tests.

Tasks:

1. Give confirmation and `ask_user` requests a structured scope containing at least channel, thread/session, and sender. Responses must match that scope.
2. Select multiple pending requests by request ID or an unambiguous scope; never choose the first item from a map.
3. Only the initiating user or a user authorized by the channel allowlist may approve a dangerous operation.
4. Block the configured credential-file path from file tools even when credential-manager initialization fails.
5. Create memory files with `0600`, tighten permissions on existing files, protect in-process access with locks, and implement forget through a same-directory temporary file plus atomic rename.
6. Limit memory recall by result count and total output size, and add concurrent-access tests.

Acceptance criteria:

- Isolation tests cover two users, two threads, and multiple pending requests.
- An unauthorized user cannot confirm or answer another user's request.
- File tools cannot read the credential file after credential decryption or initialization failure.
- Memory files end with `0600` permissions, and concurrent store/recall/forget operations preserve valid JSONL.

## Node D: Tool Metadata and Unified Policy

Owned areas: `tools/registry.go`, `agent/tool_dispatcher.go`, `agent/agent.go`, built-in tools, the MCP adapter, and related tests.

Tasks:

1. Add an optional metadata interface or registry descriptor while preserving the minimal `Tool` interface.
2. Metadata must express at least `read_only`, `idempotent`, `risk`, `default_timeout`, `network_access`, and `concurrency_key`.
3. Replace the Agent's hard-coded `readOnlyTools` set with registry metadata queries.
4. Use conservative defaults when MCP annotations are absent and map read-only/destructive annotations when provided.
5. Apply default tool timeouts in `ToolDispatcher` and expose metadata for future policy and audit consumers.
6. Add tests for unknown tools, legacy custom tools, MCP metadata, parallel read-only calls, and serialization when any call mutates state.

Acceptance criteria:

- Existing third-party and test tools compile and run without implementing the metadata interface.
- Unknown tools default to side-effecting, non-idempotent, and medium risk.
- Parallel scheduling no longer relies on a tool-name allowlist.
- `go test ./...` shows no race or behavior regression.

## Acceptance Order

1. Node D: stabilize the common tool contract first.
2. Node B: integrate network capabilities and cancellation semantics.
3. Node C: validate identity isolation and persistence safety.
4. Node A: validate the external entry point and configuration documentation.
5. Integration acceptance: full test suite, example-config parsing, README security notes, and migration notes.

## Status

- [x] Node A: HTTP API security boundary
- [x] Node B: network egress security and cancellation
- [x] Node C: IM authorization binding and sensitive data
- [x] Node D: tool metadata and unified policy
- [x] Integration acceptance

## Validation Record

Validated on 2026-09-21:

- `CGO_ENABLED=0 go test -count=1 ./...`
- `CGO_ENABLED=0 go vet ./...`
- `CGO_ENABLED=0 go build ./...`
- `gofmt` and `git diff --check`
- `example.config.toml` parsing through the repository test suite

`go test -race -count=1 ./...` could not start because this environment does not provide a C compiler (`gcc`). The required non-CGO fallback suite passed.

The native HTTP transport performs policy checks at DNS resolution and dial time. The Playwright backend intercepts browser requests, while the external `agent-browser` backend can only validate its reported current URL and quarantine a violating session; use the native or Playwright backend when dial-level enforcement is required.
