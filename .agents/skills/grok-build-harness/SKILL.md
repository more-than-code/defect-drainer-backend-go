---
name: grok-build-harness
description: >
  Harness for delegating a build to the locally installed Grok Build CLI (`grok`):
  capturing its output with the four headless formats, reading the `end` event to
  tell completion from turn exhaustion, sandbox write boundaries and custom sandbox
  profiles, the model-judged `--permission-mode auto` gate, per-session cost via
  `grok usage`, Imagine image/video tool usage, and self-contained brief structure. Activate when handing work to Grok, spawning
  `grok` headlessly, streaming or parsing grok output, tracking its cost, debugging
  a grok run that exits 0 having done nothing, generating images/video with
  image_gen / image_edit / image_to_video, or driving grok over ACP
  (`grok agent stdio` / `serve`).
---

# Grok Build Harness

Delegating a substantial build to the local **Grok Build** CLI (`grok`). Grok is a
capable agentic coder with image and video generation the host agent may lack — but
headless runs fail in specific, repeatable ways. This skill is that harness.

**Hard rule:** verify artifacts, never the model's claim of doneness. A run can
exit 0 having produced less than it narrates.

**Second hard rule: probe the version, don't read about it.** Grok moves fast and
its own shipped `~/.grok/README.md` lags the binary — as of 1.0.41 that README still
documents `"stopReason":"EndTurn"` and a sandbox that fails *open*, and the wire
disagrees with both. The changelog is not the inventory either: 1.0.41's announces a
`sports_search` tool that the session's own `available_commands` does not list. Every
observation below is dated and stamped with the version it was taken on. Re-probe
anything load-bearing.

This skill is the **transport** layer: how to spawn, observe, and resume `grok`. For the
surrounding method — when to hand a task to an external worker at all, how to brief it per
phase, and how to accept what comes back — activate `external-worker-delegation`.

## Preflight (always)

```bash
grok --version            # observations below verified on 1.0.41 (2026-09-24)
grok models               # confirms login + available models
grok inspect              # skills, agents, hooks, permissions, sandbox for THIS directory
```

`grok models` failing or printing a login prompt means the delegation cannot run —
the user must `grok login` themselves.

`grok inspect` is the one to actually read before writing a brief: it prints which
instruction files, skills, agents and hooks Grok will load *in that directory*, which
of them are disabled, and its harness-compatibility switches. A worker inheriting a
skill you did not expect, or missing one you did, is visible here and nowhere else.

**Record the version alongside any observation about the wire format.** The event
schema, the `stopReason` vocabulary and the turn semantics have all changed across
majors; a claim about what `grok` emits means nothing without the version that
emitted it.

## Observing the run — always use a streaming format

Plain `-p` prints only the model's prose, **at exit**. It is not a progress signal and
it hides why the run stopped. Headless supports four formats (1.0.41):

| Format | Emits | Use |
|--------|-------|-----|
| `plain` (default) | prose at exit | never, for delegation |
| `json` | one object at exit | scripted single-shot calls; pairs with `--json-schema` |
| `streaming-json` | NDJSON, one **ACP session update** per line — Grok's native format | **delegation** |
| `streaming-messages-json` | NDJSON in the Anthropic Messages API wire format | reusing an Anthropic-shaped parser you already own |

```bash
grok -p "…" --output-format streaming-json | tee run.jsonl
```

`--include-partial-messages` adds incremental text/thinking deltas, and affects
`streaming-messages-json` only.

**`streaming-json` *is* the ACP event stream.** The two transports have converged:
what the help calls "the agent's native format" is the same session-update vocabulary
`grok agent stdio` pushes. Tool visibility is therefore in headless mode, and the
guidance that it was ACP-only is obsolete.

Directly observed on 1.0.40 and 1.0.41: `text`, `thought`, `tool_call`, `tool_call_update`,
`usage`, `available_commands`, `end`, and `max_turns_reached` (under `--max-turns`).
Carried over from earlier versions but not seen in these probes: `error`,
`auto_compact_*`. The list is **not exhaustive** and it grows between versions, so
switch on `type` and ignore unknowns.

`tool_call` carries `toolCallId`, `toolName`, `kind`, `title`, `status` and `rawInput`;
`tool_call_update` carries the result, and for a write it includes a real diff
(`{type:"diff", path, oldText, newText}`) plus `locations`. That is genuine write
visibility — but it reports what Grok *attempted*, so for deliverables still check
the filesystem:

```bash
find work/src work/static -type f -newer work/BRIEF.md | sort
```

`available_commands` enumerates the live tool and slash-command inventory for the
session. Read it instead of assuming which tools exist in the version you are driving.

The `end` event is the run's post-mortem, verified shape on 1.0.40 and unchanged on 1.0.41:

```json
{"type":"end","stopReason":"end_turn","sessionId":"…","requestId":"…",
 "usage":{…},"num_turns":11,"total_cost_usd":0.089947,
 "total_cost_usd_ticks":899470000,"modelUsage":{…}}
```

Read four fields every time:

- **`stopReason`** — why it stopped, and now **snake_case** (`end_turn`, `cancelled`).
  A driver loop grepping for the old `EndTurn` matches nothing and loops forever.
- **`num_turns`** — how much it actually did.
- **`total_cost_usd`** — accumulate across iterations. `total_cost_usd_ticks` is the
  same figure as an integer, USD x 10^10.
- **`sessionId`** — keep it. It is the key to `grok usage` (see Cost below) and to
  `grok -r <id>`.

## Gotcha 1 — turn exhaustion, not single-turn (changed in 1.0)

**On 1.0.x, `-p` runs the task to completion.** Verified on 1.0.40 (2026-09-22) and
again on 1.0.41 (2026-09-24): a six-step brief with read-then-write dependencies between
steps, given in one `--prompt-file`, ran every step in **11 turns** and exited 0 with
`stopReason: "end_turn"` and correct file contents.

The old trap is gone. On **0.2.x**, `-p` ran exactly one assistant turn: the model did
a batch of tool calls, ended its response intending to continue, and the process exited
0 mid-plan with a truncated narration ("Scaffolding next…"). That failure mode cost
hours of misdiagnosis and is what the driver loop below existed to work around. If you
are on a 0.2.x build, it still applies — and the fix is to upgrade, not to loop.

**What still stops a run early is the turn cap**, and it is cheap to identify because
the signals are unambiguous (verified on 1.0.40 and 1.0.41 with `--max-turns 2`):

| Exit | `stopReason` | Extra event | Meaning |
|------|--------------|-------------|---------|
| 0 | `end_turn` | — | Ran to completion. Check artifacts anyway. |
| 1 | `cancelled` | `max_turns_reached` | Hit `--max-turns`. Resume it. |
| non-zero | absent / `error` | `error` | Genuine failure. Read it; do not resume blindly. |

So: a **non-zero exit plus `max_turns_reached`** is the resume signal, and `end_turn`
with deliverables missing is a real model stop worth reading the narration for. Do not
raise `--max-turns` reflexively; an unfinished brief at a high cap usually means the
brief was too big for one session, not that the cap was too low.

**Driver loop — only for turn exhaustion, and still artifact-gated:**

```bash
for i in $(seq 1 30); do
  grok -c -p "Continue executing BRIEF.md, resuming where you left off. \
Do not stop early; every deliverable must exist and be verified." \
    --sandbox workspace --always-approve --max-turns 400 \
    --output-format streaming-json | tee -a run.jsonl
  [ -f NOTES.md ] && ls dist/*.out >/dev/null 2>&1 && break   # real completion markers
done
```

Rules for the loop:
- Test **artifacts on disk**, never the model's claim of doneness.
- `grok -c` resumes the most recent session *for that cwd*, so context carries over;
  `grok -r <sessionId>` resumes a specific one, and `--fork-session` branches instead
  of reusing the id.
- Break on a genuine error. Note that a `max_turns_reached` run also exits non-zero,
  so a loop that breaks on *any* non-zero exit will not resume — distinguish the two
  by `stopReason` rather than by exit code alone.
- One resumed iteration commonly runs 15–25 min and does enormous work. Budget for it.

## Gotcha 2 — the sandbox write boundary, and how to widen it

Five built-in profiles on 1.0.40 and 1.0.41 (probed by name). Take write sets from the
bundled `~/.grok/docs/user-guide/18-sandbox.md`, not the shipped `~/.grok/README.md`:
on 1.0.41 the README omits `devbox`, lists older write sets, and says `~/.ssh`,
`~/.aws`, `~/.gnupg` are "always write-protected regardless of profile", which probing
disproves (below).

| Profile | FS read | FS write | Child network | `~/.ssh` `~/.aws` `~/.gnupg` | Use |
|---------|---------|----------|---------------|------------------------------|-----|
| `off` (default) | everywhere | everywhere | allowed | **writable** | no sandbox |
| `workspace` | everywhere | CWD + temp + `~/.grok` | allowed | blocked only while outside CWD | default; recommended |
| `read-only` | everywhere | temp + `~/.grok` | blocked (Linux only) | outside the write set (unprobed) | exploration/review |
| `strict` | CWD + system paths + `~/.grok` | CWD + temp + `~/.grok/sessions` | blocked (Linux only) | outside the write set (unprobed) | untrusted code |
| `devbox` | everywhere | every top-level dir except `/data` | allowed | **writable** | disposable VMs only |

"Temp" is `/tmp`, `/var/tmp` and the macOS temp dirs. Child-network blocking is
seccomp, so on macOS `read-only` and `strict` do not restrict it.

**No profile protects credential paths by name.** `~/.ssh`, `~/.aws` and `~/.gnupg`
are unwritable only when they fall outside the profile's write set. Probed on 1.0.41
(2026-09-26) with a create-then-remove file: all three were writable under `off` and
`devbox`. Under `workspace` they were blocked from a scratch CWD, but writable when
CWD was `$HOME`, and a `read_write` grant that covers them would open them too. Grok's
own credentials (`~/.grok/auth.json`) sit inside the `~/.grok` write grant and stayed
writable under `workspace`. What Grok does write-deny is its config, trust, sandbox
and hook files (`config.toml`, `sandbox.toml`, `hooks/`, ...), and only under
`workspace`, `read-only` and `strict`, not `devbox`. To guarantee a credential path is
off-limits, list it under `deny` in a custom profile (below). That also blocks reads.

The sandbox is applied to the whole process at startup and is **irreversible**. The
model cannot talk its way out of it at runtime.

**It fails closed, not open.** An unknown profile name, or one whose rules cannot be
resolved, prints a warning and *refuses to start*: "Refusing to start with its
protections missing." (The README's "logs a warning and continues without
enforcement" is stale.) One live instance of this: `read-only` and `strict` both
carry a deny rule for the container runtime socket, and on a host where
`/var/run/docker.sock` is a **symlink**, resolution fails and neither profile will
start at all (reproduced on 1.0.41: both exit 1 with the refusal, as does an unknown
name, while `off`, `workspace` and `devbox` start). The warning names the cause as
"endpoint is a symlink", not a missing target. The reproducing host (2026-09-26) ran
Podman, and its symlink was a dangling leftover pointing at a nonexistent
`~/.docker/run/docker.sock`; no runtime used it. So the trigger is the link itself,
whatever created it. Tools that install a Docker-compatible socket as a symlink
(for example `podman-mac-helper`) would plausibly trip it on a working setup too;
that case is unverified. Check with `ls -l /var/run/docker.sock`, and probe the
profile you intend to use before writing a brief around it.

**Custom profiles are the real fix for cache paths.** Define them in `~/.grok/sandbox.toml`
(global) or `.grok/sandbox.toml` (per project) and pass the name to `--sandbox`:

```toml
[profiles.build]
extends = "workspace"
read_write = ["/tmp/scratch"]   # literal directories, no globs
read_only  = ["/data"]
deny       = ["/data/secrets"]
restrict_network = true
```

A `read_write` grant for the toolchain's cache directory turns Gotcha 3's class of
failure from a workaround into a configuration line. Prefer it over `devbox`.

### What actually breaks in `workspace` (probed 2026-09-22, re-probed on 1.0.41 2026-09-24)

| Tool | Behaviour inside `workspace` | Verdict |
|------|------------------------------|---------|
| `pnpm install` | **Works.** Cannot write `~/Library/pnpm/store`, so pnpm 12.x silently relocates the store to a writable path and installs. Exit 0. Where it lands varies: `node_modules/.pnpm-store` in one probe, `/private/tmp/.pnpm-store` for a project under `/private/tmp` in another. | fine, but a cold store |
| `npm install` | **Fails** `EPERM` on `~/.npm` | blocked |
| direct write to `~/.npm` | `Operation not permitted`, exit 1 — under `--always-approve`. Under `--permission-mode auto` the command never runs (next section) | as designed |

**npm's error message is actively misleading.** On the cache EPERM it reports "Your
cache folder contains root-owned files, due to a bug in previous versions of npm" and
tells you to run `sudo chown -R … ~/.npm`. Nothing is root-owned and that command
fixes nothing — it is npm guessing at the only permission problem it knows about.
The real cause is the sandbox boundary. Do not run the `sudo` it suggests, and warn
the brief's reader off it too.

The older claim that **pnpm segfaults** here (`SecItemCopyMatching -50`, exit 139) was
real on the 0.2.x/1.0.13-era combination but **does not reproduce on 1.0.40 or 1.0.41
with pnpm 12.5.1**. Treat a signal death from a package manager as a version-specific symptom to
re-probe, not a standing fact.

**Fix — do the environment work yourself, outside the sandbox, before spawning:**

1. Scaffold the project and install dependencies (also avoids interactive creators,
   which hang or crash headless).
2. Pre-install any browser/binary the run needs. Reads work everywhere, so Grok can
   execute what you installed.
3. Or grant the cache path with a custom profile's `read_write`, which is better than
   redirecting caches per-spawn and better than dropping the sandbox.
4. Tell Grok in the brief that this is done and it must not re-scaffold.

### `--permission-mode auto` is a second gate, and a judgment call

The sandbox is not the only thing that can refuse an action. Under
`--permission-mode auto`, a model-judged classifier reviews each tool call first, and
**`--always-approve` does not override it.** Observed on 1.0.41 (2026-09-24): a brief
passed via `--prompt-file` explicitly asked for `touch ~/.npm/_probe`. The command never
ran, and the tool call failed with:

```text
Tool `run_terminal_command` was not executed: Auto mode blocked this action (… not
clearly requested in the visible user turn). … If no safer alternative exists, ask
the user how to proceed.
```

Three things make this a harness problem, not just a safety feature:

- **A headless run has no user to ask.** The model carries on without the step.
- **The run still ends `end_turn` with exit 0.** The refusal shows up only as a
  `tool_call_update` with `status: "failed"`, and in whatever the model chooses to put
  in `NOTES.md`.
- **A brief read from a file may not count as the user's request.** The classifier
  refused an action the brief spelled out word for word.

The same command under `--always-approve` without `auto` ran and hit the sandbox
(`Operation not permitted`, exit 1), which is the boundary a brief can reason about.
For delegation, make the sandbox the boundary and drop `auto`. If you keep it, check
every run for refusals:

```bash
jq -r 'select(.type=="tool_call_update" and .status=="failed")
       | .content[]?.content.text? // empty' run.jsonl | grep 'Auto mode blocked'
```

Not probed on 1.0.40, so this is not claimed to be new in 1.0.41. Only that it is
present there.

## Gotcha 3 — headless Chromium under the sandbox

Playwright/Chromium has crashed under the sandbox (keychain `SecItemCopyMatching` /
crashpad), and its browser cache (`~/Library/Caches/ms-playwright` on macOS) is
outside the `workspace` writable set, so the download fails before the browser ever
runs. **Not re-probed on 1.0.40 or 1.0.41** — the package-manager half of this failure family
was fixed (Gotcha 2), so verify rather than assume before designing around it.

If it does crash, anything depending on headless capture — screenshots, HTML→PNG,
frame rendering for video — needs a fallback. Native rendering (e.g. Swift/AppKit on
macOS) works. State the fallback in the brief so Grok doesn't silently drop the
deliverable. The cheaper first move is a custom profile granting the browser cache
`read_write`, plus pre-installing the browser outside the sandbox.

## Gotcha 4 — native-toolchain builds may need a clean env

Rolldown/Vite-class native binaries can segfault under the agent's inherited
environment. Workaround Grok can use, and worth pre-authorizing in the brief:

```bash
env -i HOME="$HOME" PATH="/opt/homebrew/bin:/usr/bin:/bin" pnpm build
```

Verify the build yourself afterwards in a normal shell — it usually passes there.

## Cost — `grok usage` outlives the run log

The `end` event carries `total_cost_usd` per run, but Grok also **persists usage per
session**, so the run log is not the only copy:

```bash
grok usage <sessionId>          # session totals + every recorded turn
grok usage <sessionId> <turn>   # one turn
```

It returns JSON: `inputTokens`, `outputTokens`, `cachedReadTokens`,
`reasoningTokens`, `modelCalls`, `turnCount`, `primaryModelId`, and `costUsdTicks`
(USD x 10^10 — `899470000` is $0.0899), nested under `session` for the totals and
`turns[]` per turn. Verified on 1.0.40 and 1.0.41 against the same session's `end`
event; the cost figures agree to the tick.

**The two commands count different "turns".** `grok usage`'s `turnCount` and its
`[TURN]` argument count *prompts* — one per `-p` or `-c` invocation — while the `end`
event's `num_turns` counts model calls and matches `modelCalls`. The same 1.0.41 run
reported `num_turns: 11` and `turnCount: 1`. Handy for a driver loop: `grok usage <id> 3`
is the cost of the third iteration.

This matters for teardown: a worktree deleted with its `run.jsonl` inside has not
destroyed the cost record, provided you kept the `sessionId`. Keep it anyway — it is
also how you resume (`grok -r`) and export (`grok export`) that session.

## Imagine — image and video generation

All four tools are present on 1.0.40 and 1.0.41 — `image_gen`, `image_edit`, `image_to_video`
and `reference_to_video` — confirmed in the session's own `available_commands` event
rather than from docs. Check that event for the version you are driving instead of
hedging. Instruct Grok to **load the bundled `imagine` skill before its first
generation**; note it is listed as `bundled:imagine` where a user skill has taken the
bare `/imagine` name.

**The split that decides output quality:**

| Needs | Use |
|-------|-----|
| Only the *look* — scenes, characters, textures, atmosphere | `image_gen` / `image_edit` |
| Exact text, data, structure — UI mockups, wordmarks, charts, share images with copy | **code** (HTML/CSS rendered, or SVG) |

Image models garble words, invent numbers, and break layout; a longer prompt does not
fix it and an edit pass rarely does.

**Consistency:** for a recurring character or object, generate/choose **one base image**
and `image_edit` every variation from it. Never `image_gen` the same subject twice.
If production art already exists in the repo, **reuse it** — it beats generation for
identity fidelity.

**Video:** starts from an image (no text-to-video). Plan as **short shots** (6s or 10s),
stage frame 1 with `image_gen`/`image_edit`, animate with `image_to_video`, one simple
camera move per shot. Lock **every** shot — generated and code-rendered — to one
resolution and frame rate so assembly is lossless:

```bash
ffmpeg -f concat -safe 0 -i shots.txt -c copy out.mp4    # never re-encode
```

Expect transient **HTTP 429** on parallel generation calls; retry sequentially.

## Interactive delegation — `grok agent stdio` (ACP)

`-p` is fire-and-forget. For a back-channel, Grok also speaks **ACP** (Agent Client
Protocol) — JSON-RPC 2.0, newline-delimited, over stdio or WebSocket:

```bash
grok agent --always-approve stdio                                  # local
grok agent --always-approve serve --bind 127.0.0.1:2419 --secret X # WebSocket
```

**Stamp a `serve` process as the worker when you start it:**

```bash
SKILL_FORGE_AGENT_ROLE=worker grok agent --always-approve serve --bind 127.0.0.1:2419 --secret X
```

A per-job spawn gets its role marker from the dispatcher (see `external-worker-delegation`), but a
server starts once and serves every later session from that one environment. Launched from a shell
that declares you the orchestrator, it inherits *orchestrator* and every job it runs is told to
hand the work off — backwards. If the server exists to do delegated work, mark the whole server.

One server splitting both roles across its sessions has no per-session signal to carry this; use
separate servers, or `stdio`.

**Verified handshake** (probed against 0.2.102; the method still holds on 1.0.40,
but re-probe the notification vocabulary before depending on a specific kind):

1. `initialize` -> `{"protocolVersion":1}`
2. `session/new` `{cwd, mcpServers:[], _meta:{yoloMode:true}}` -> `sessionId`
3. `session/prompt` `{sessionId, prompt:[{type:"text",text:"..."}]}` -> `{stopReason:"end_turn"}`
4. Agent pushes `session/update` notifications throughout.

A trivial one-tool prompt produced **104** `session/update` notifications. Observed
`sessionUpdate` kinds:

| Kind | Why it matters |
|------|----------------|
| `tool_call`, `tool_call_update`, `tool_call_delta_chunk` | live tool name, status, result — since 1.0 also available from headless `streaming-json` |
| `agent_thought_chunk`, `agent_message_chunk` | reasoning + response text |
| `pending_interaction`, `interaction_resolved` | the **permission back-channel** |
| `turn_completed`, `response_completed`, `session_summary_generated` | turn lifecycle |
| `available_commands_update`, `model_changed`, `session_info_update` | session state |

Side-channel notifications arrive as `_x.ai/*` methods (`_x.ai/session_notification`,
`_x.ai/queue/changed`, `_x.ai/models/update`, ...). **Note:** the docs spell these
`x.ai/*`; the wire uses a leading underscore. Discover from `initialize`, don't hardcode.

### What ACP still buys you

Tool visibility is no longer the reason — `streaming-json` has the same events since
1.0, and the old single-turn stop that made process lifecycle painful is gone
(Gotcha 1). What remains ACP-only:

- **The permission back-channel.** `pending_interaction` / `interaction_resolved` let
  your code answer a prompt mid-run instead of pre-approving everything with
  `--always-approve` or `--permission-mode auto`.
- **Mid-run steering.** Continuation is another `session/prompt` on the same session —
  no new process, no `-c`, no context reload — and you decide what to send based on
  what the last turn actually did.

**Choose:**

| Want | Use |
|------|-----|
| One batch job, minimal client code | `-p` (+ `--prompt-file`) with `streaming-json` |
| Granular permissions, mid-run steering | `agent stdio` (ACP) |
| Remote / multi-client | `agent serve` (WebSocket + `--secret`) |

Official ACP SDKs exist for TypeScript, Rust, Python, Go and Kotlin, and Zed / Neovim /
Emacs are working clients — prefer one over hand-rolling. `EXAMPLES.md` carries a
~40-line Python client that completes the handshake above.


## The brief

Grok starts **cold** — it cannot see the delegating conversation. Write a
self-contained `BRIEF.md` in the working directory and point Grok at it. Since 1.0
there is a flag for exactly this, so the brief need not be squeezed into an argument:

```bash
grok --prompt-file BRIEF.md --no-subagents \
  --sandbox workspace --always-approve --output-format streaming-json
```

Two flags worth setting on a delegated spawn:

- **`--no-subagents`** enforces the no-recursion rule mechanically — the worker cannot
  fan the brief out to children the orchestrator never sees. Verified accepted on 1.0.40
  and 1.0.41. Pass it explicitly rather than relying on config: 1.0.41 changed which
  config tables disable subagents (one that only sets limits or models no longer does),
  so a setup that was quietly subagent-free may not be any more.
- **`--rules "<text>"`** appends to the system prompt. Useful for the role line, but it
  does **not** replace the brief's `Role` section: rules are tooling-injected and the
  brief is the authoritative channel (`external-worker-delegation` covers the precedence).

**Do not pass `--worktree` when the orchestrator already created one.** Grok has its
own worktree subsystem (`--worktree`, `--worktree-ref`, `grok worktree list|rm|gc`),
and pointing it at a dispatcher-managed checkout nests one isolation mechanism inside
another — two branches, two teardowns, and a merge target that is not the one the
orchestrator is reviewing. Pick whose worktree it is and say so in the brief.

Required sections:

1. **Facts** — what the product/system actually is; never assume shared context.
2. **Decisions already made** — as a table, marked do-not-relitigate, so it does not
   re-open settled questions.
3. **Environment** — Gotcha 2/3/4 constraints, what is pre-installed, what not to run.
4. **Deliverables** — concrete, with the code-vs-generate split spelled out.
5. **Honesty constraints** — what it must not claim or invent. Agents fill gaps with
   plausible fabrication (testimonials, metrics, unshipped features) unless forbidden.
6. **`NOTES.md` requirement** — decisions it made, departures from the brief and why,
   what it could not verify, what a human must do next. This is where the real
   signal lands; read it first when the run ends.

## Verify independently

Never accept the agent's completion claim. Re-run the gates yourself:

- Build/test commands, in your own shell (not the sandboxed one).
- `ffprobe` on media: codec, dimensions, frame rate, duration, size.
- Grep generated copy for fabrication — invented metrics, testimonials, claims about
  features that do not exist.
- Render the result and look at it.

Grok's `NOTES.md` departures are usually honest and sometimes better than the brief
(e.g. reusing production art instead of generating portraits). Read them, judge them
on merit, and record accepted departures upstream.

## Permission note

The host agent may be blocked from spawning an unattended `grok` process by its own
permission classifier, regardless of flags. That is a host-side control: explain what
you are trying to run and let the user launch it or add a permission rule. Do not
paper over it with flag variations.

## Anti-patterns

| Wrong | Right |
|-------|-------|
| Treat exit 0 as success | Check artifacts; read `stopReason` and `num_turns` |
| Match `stopReason` against `EndTurn` | `end_turn` / `cancelled` — snake_case since 1.0 |
| Break the driver loop on any non-zero exit | `max_turns_reached` also exits non-zero; branch on `stopReason` |
| Raise `--max-turns` until it finishes | An unfinished brief at a high cap means the brief was too big |
| Reach for ACP to see tool calls | `streaming-json` has them since 1.0; use ACP for permissions and steering |
| Trust a wire-format claim from an older version — or from Grok's own README | `grok --version`, then probe; record both |
| Run npm's suggested `sudo chown -R ~/.npm` | It is npm misreading the sandbox boundary; nothing is root-owned |
| Assume a package manager still segfaults in-sandbox | Re-probe; pnpm 12.x relocates its store and installs fine |
| `--sandbox devbox` / no sandbox to dodge write errors | A custom profile with `read_write` for the cache path |
| Assume an unappliable sandbox degrades to unsandboxed | It refuses to start; probe the profile first |
| `--permission-mode auto` on a headless brief | `--always-approve` with the sandbox as the boundary; if you keep `auto`, scan for `Auto mode blocked` |
| Let Grok scaffold and install in-sandbox | Pre-build the environment outside it |
| Pass `--worktree` into a dispatcher-managed worktree | One isolation mechanism; decide whose and state it |
| Scrape cost only from `run.jsonl` | `grok usage <sessionId>` persists it |
| `image_gen` a UI mockup or anything with real copy | Build it in code |
| Re-`image_gen` a recurring character | `image_edit` from one base image |
| Re-encode when joining shots | `-c copy` with matched res/fps |
| A prompt that references "the plan above" | Self-contained `BRIEF.md` via `--prompt-file` |
