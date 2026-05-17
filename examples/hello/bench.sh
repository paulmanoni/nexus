#!/usr/bin/env bash
# bench.sh — measure end-to-end framework boot time for a
# trivial hello-world live-template app.
#
# Reported number is wall-clock from `exec` of the binary
# until the first HTTP/200 from GET /. That covers:
#   - Go runtime init
#   - fx graph build + provider invocations
#   - dashboard route registration
#   - gin engine init + middleware stack
#   - net.Listen + accept loop ready
#   - template.Engine register + component lowering
#
# Run after `go build -o /tmp/hello ./examples/hello`.

set -euo pipefail

BIN="${BIN:-/tmp/hello}"
PORT="${PORT:-8083}"
ITERS="${ITERS:-5}"

if [[ ! -x "$BIN" ]]; then
    echo "binary not found at $BIN — run: go build -o $BIN ./examples/hello" >&2
    exit 1
fi

samples=()
for i in $(seq 1 "$ITERS"); do
    lsof -ti:"$PORT" 2>/dev/null | xargs -r kill -9 2>/dev/null || true

    start_ms=$(python3 -c 'import time; print(int(time.time()*1000))')

    "$BIN" > /tmp/hello.run.log 2>&1 &
    pid=$!

    while :; do
        code=$(curl -sS -o /dev/null -w '%{http_code}' \
            --connect-timeout 1 --max-time 1 \
            "http://127.0.0.1:${PORT}/" 2>/dev/null || echo 000)
        if [[ "$code" == "200" ]]; then break; fi
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "iter $i: binary exited before serving — see /tmp/hello.run.log" >&2
            exit 1
        fi
        now_ms=$(python3 -c 'import time; print(int(time.time()*1000))')
        if (( now_ms - start_ms > 5000 )); then
            echo "iter $i: timeout (>5s)" >&2
            kill "$pid" 2>/dev/null || true
            exit 1
        fi
        python3 -c 'import time; time.sleep(0.005)'
    done

    end_ms=$(python3 -c 'import time; print(int(time.time()*1000))')
    elapsed=$((end_ms - start_ms))
    samples+=("$elapsed")
    echo "iter $i: ${elapsed} ms"

    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    sleep 0.2
done

sorted=$(printf '%s\n' "${samples[@]}" | sort -n)
n=$(printf '%s\n' "$sorted" | wc -l | tr -d ' ')
mid=$(( (n + 1) / 2 ))
median=$(printf '%s\n' "$sorted" | sed -n "${mid}p")

echo
echo "median: ${median} ms over ${ITERS} runs"
