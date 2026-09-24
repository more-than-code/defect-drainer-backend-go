# EXAMPLES — Grok Build harness

Copy-paste sequences. Paths are illustrative; substitute your own.

## 1. Preflight

```bash
grok --version && grok models && grok inspect
```

If `grok models` prompts for login, stop — the user must authenticate.

## 2. Prepare the working directory (outside the sandbox)

```bash
TARGET=/path/to/work
mkdir -p "$TARGET" && git -C "$TARGET" init -q

# Scaffold + install yourself — interactive creators crash headless, and npm's
# cache (~/.npm) is outside the sandbox's writable set (pnpm relocates and survives).
npx -y sv@latest create --template minimal --types ts --no-install --no-add-ons /tmp/scaffold
cp -R /tmp/scaffold/. "$TARGET"/
cd "$TARGET" && pnpm add -D <deps> && pnpm build     # prove it builds BEFORE delegating

# Pre-install any browser the run needs (reads work inside the sandbox)
npx playwright install chromium
```

Then write `BRIEF.md` into `$TARGET` (see SKILL.md § The brief) and commit it, so
Grok starts from a clean, known baseline.

## 3. Spawn — one shot first

On 1.0.x a single `-p` runs the brief to completion; the loop in 3a is for turn
exhaustion, not the default.

```bash
cd /path/to/work || exit 1
grok --prompt-file BRIEF.md --no-subagents \
  --sandbox workspace --always-approve --max-turns 400 \
  --output-format streaming-json | tee run.jsonl
```

## 3a. Driver loop — only when a run hits `--max-turns`

```bash
#!/bin/zsh
cd /path/to/work || exit 1

PROMPT='Continue executing BRIEF.md end to end, resuming exactly where you left off.
Do not stop early: every deliverable must exist and be verified. Run the build and
the media checks before you consider yourself done, and write NOTES.md.'

for i in $(seq 1 30); do
  echo "########## ITERATION $i $(date +%H:%M:%S) ##########"
  grok -c -p "$PROMPT" --sandbox workspace --always-approve --max-turns 400 \
    --output-format streaming-json | tee -a run.jsonl
  # Completion markers = artifacts on disk, not the model's word
  if [ -f NOTES.md ] && ls static/video/*.mp4 >/dev/null 2>&1; then
    echo "=== COMPLETE after iteration $i ==="; break
  fi
  # A turn-cap stop exits non-zero too, so branch on stopReason, not on rc.
  last=$(grep '"type":"end"' run.jsonl | tail -1)
  case "$last" in
    *'"stopReason":"cancelled"'*) echo "turn cap; resuming" ;;
    *) echo "stopped for another reason; inspect run.jsonl"; break ;;
  esac
done
```

## 3b. Reading the output stream

Add `--output-format streaming-json` and tee to a log. Live human-readable narration:

```bash
grok -c -p "$PROMPT" --output-format streaming-json --sandbox workspace \
  | tee -a run.jsonl \
  | python3 -u -c "
import sys, json
for line in sys.stdin:
    try: o = json.loads(line)
    except ValueError: continue
    t = o.get('type')
    if t == 'text':  sys.stdout.write(o.get('data',''))
    elif t == 'end': print('\n[end]', o.get('stopReason'), 'turns=', o.get('num_turns'), 'cost=\$%.4f' % o.get('total_cost_usd', 0))
    elif t == 'error': print('\n[error]', o.get('message'))
"
```

Post-mortem of any completed run, and total spend across a driver loop:

```bash
# Why did each iteration stop, and how much did it do?
grep '"type":"end"' run.jsonl | python3 -c "
import sys, json
tot = 0.0
for i, l in enumerate(sys.stdin, 1):
    o = json.loads(l); c = o.get('total_cost_usd', 0) or 0; tot += c
    print(f\"iter {i}: {o.get('stopReason')} turns={o.get('num_turns')} \${c:.4f}\")
print(f'TOTAL \${tot:.4f}')
"
```

On 1.0.x: `stopReason: end_turn` = ran to completion (check artifacts anyway);
`stopReason: cancelled` alongside a `max_turns_reached` event = hit `--max-turns`,
so resume. The value is snake_case — matching `EndTurn` finds nothing.

`tool_call` / `tool_call_update` give live write visibility, including diffs:

```bash
python3 -c "
import json,sys
for l in open('run.jsonl'):
    o=json.loads(l)
    if o.get('type')=='tool_call': print(o['toolName'], json.dumps(o.get('rawInput'))[:120])
"
```

They report what Grok *attempted*, so confirm deliverables on disk:

```bash
find work/src work/static -type f -newer work/BRIEF.md | sort
```

Cost survives the log — keep the `sessionId` from the `end` event:

```bash
grok usage <sessionId>     # costUsdTicks = USD x 10^10
```

## 4. First run of a fresh session

The loop's `-c` needs an existing session for that cwd. Either seed one:

```bash
grok -p "Read BRIEF.md and begin executing it." --sandbox workspace --always-approve
```

…then run the loop, or make iteration 1 use the seed prompt and later ones `-c`.

## 5. Verify — independently of Grok's claims

```bash
cd /path/to/work
pnpm build; echo "BUILD EXIT: $?"          # your shell, not the sandboxed one

ffprobe -v error -show_entries format=duration,size \
  -show_entries stream=width,height,r_frame_rate,codec_name \
  -of default=noprint_wrappers=1 static/video/hero.mp4

# Fabrication sweep over generated copy
grep -rioE "app store|[0-9,]+\+? (users|customers)|trusted by|testimonial|★|\\\$[0-9]" build/
```

Then read `NOTES.md` — departures and unverifiable items land there.

## 6. Media recipes

```bash
# Assemble shots losslessly (all must share resolution + fps)
printf "file '%s'\n" shots/*.mp4 > /tmp/shots.txt
ffmpeg -f concat -safe 0 -i /tmp/shots.txt -c copy hero.mp4

# Web encodes from one master
ffmpeg -i hero.mp4 -c:v libx264 -pix_fmt yuv420p -movflags +faststart out.mp4
ffmpeg -i hero.mp4 -c:v libvpx-vp9 -b:v 0 -crf 34 out.webm
ffmpeg -i hero.mp4 -vframes 1 -q:v 3 poster.jpg
```

Note: some ffmpeg builds lack a **libwebp** encoder — check before promising WebP
rasters (`ffmpeg -encoders | grep webp`), and fall back to PNG/JPEG or `sips`.

## 7. Diagnosing a run that "did nothing"

| Symptom | Cause | Action |
|---------|-------|--------|
| Non-zero exit, `stopReason: cancelled`, `max_turns_reached` | turn cap | resume with `-c` |
| Exit 0, `end_turn`, deliverables missing | model stopped early | read the narration; tighten the brief |
| Exit 0, truncated narration, few files, **0.2.x** | single-turn `-p` | upgrade; else driver loop with `-c` |
| Crash in a create/scaffold CLI | cache write outside sandbox | pre-scaffold outside |
| `EPERM` on `~/.npm`, npm blames root-owned files | sandbox write boundary | ignore npm's `sudo chown` advice; install outside, or grant `read_write` |
| Refuses to start, "protections missing" | sandbox profile unappliable | probe the profile; check `/var/run/docker.sock` is not a symlink |
| Chromium SIGSEGV | sandbox + keychain/crashpad | native renderer fallback |
| Build segfault only under agent | inherited env | `env -i HOME=… PATH=… <build>` |
| HTTP 429 during generation | parallel Imagine calls | retry sequentially |

## 8. Minimal ACP client (`grok agent stdio`)

Verified against grok 0.2.102 (re-probe the notification kinds on newer builds).
Completes initialize → session/new → session/prompt and
prints every `tool_call` as it happens. Since 1.0 headless `streaming-json` carries
the same events, so reach for this client when you need the permission back-channel or
mid-run steering, not merely tool visibility.

```python
import json, subprocess, threading, time, collections

CWD = "/path/to/work"
proc = subprocess.Popen(["grok", "agent", "--always-approve", "stdio"], cwd=CWD,
    stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)

results, updates, lock = {}, [], threading.Lock()

def reader():
    for line in proc.stdout:
        line = line.strip()
        if not line: continue
        try: m = json.loads(line)
        except ValueError: continue
        with lock:
            if "id" in m and ("result" in m or "error" in m): results[m["id"]] = m
            elif m.get("method"): updates.append(m)
threading.Thread(target=reader, daemon=True).start()

def send(i, method, params):
    proc.stdin.write(json.dumps(
        {"jsonrpc": "2.0", "id": i, "method": method, "params": params}) + "\n")
    proc.stdin.flush()

def wait(i, secs=300):
    t0 = time.time()
    while time.time() - t0 < secs:
        with lock:
            if i in results: return results[i]
        time.sleep(0.1)

send(1, "initialize", {"protocolVersion": 1,
     "clientCapabilities": {"fs": {"readTextFile": False, "writeTextFile": False}}})
wait(1)

send(2, "session/new", {"cwd": CWD, "mcpServers": [], "_meta": {"yoloMode": True}})
sid = wait(2)["result"]["sessionId"]

send(3, "session/prompt", {"sessionId": sid,
     "prompt": [{"type": "text", "text": "Read BRIEF.md and begin executing it."}]})
print("stopReason:", wait(3)["result"]["stopReason"])

# Continue the SAME session — no new process, no `-c`
send(4, "session/prompt", {"sessionId": sid,
     "prompt": [{"type": "text", "text": "Continue until every deliverable exists."}]})
wait(4)

with lock:
    kinds = collections.Counter(
        (u.get("params", {}).get("update") or {}).get("sessionUpdate") for u in updates)
print(dict(kinds))
```

Live tool feed — replace the counter with:

```python
u = upd.get("params", {}).get("update", {})
if u.get("sessionUpdate") in ("tool_call", "tool_call_update"):
    print("TOOL:", u.get("title"), u.get("status"))
```

Watch for `pending_interaction`: the agent is asking permission. Answer it instead of
pre-approving everything with `yoloMode` when the run touches anything destructive.
