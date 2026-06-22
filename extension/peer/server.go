package peer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/di"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/trace"
)

// callEntry is one method registered via AsCall. Bound at di.Start
// after fx has resolved every dep the user's handler declared; read
// by the inbound dispatcher on every /__peer/call request.
type callEntry struct {
	Method   string
	Bound    nexus.BoundHandler
	ArgsType reflect.Type // may be nil for no-arg methods
	RetType  reflect.Type // may be nil for void/error-only handlers
}

// callTable is the process-wide method registry, populated at boot
// by every peer.AsCall invocation. Singleton because the inbound
// dispatcher built in mountServer needs a single shared view; making
// this per-Module would mean threading state through fx providers
// that the user shouldn't see.
var callTable sync.Map // string method → *callEntry

// AsCall registers a method callable from peer apps. The handler
// follows the same reflective shape every other nexus registration
// accepts (AsRest, AsQuery, AsMutation):
//
//	func(svc *Service, deps..., p nexus.Params[Args]) (Result, error)
//
// At di.Start, every dep is resolved once and bound into a closure;
// the dispatcher invokes that closure on each inbound /__peer/call,
// decoding the JSON body into Args and JSON-encoding the Result.
//
// The method name is the public identifier peers spell when calling
// in via peer.Call. Choose a stable name; renaming is a breaking
// wire change.
//
//	peer.AsCall("createOrder", NewCreateOrder)
//
// Method names must be unique within the process — duplicate
// registrations fail at boot with a clear error.
func AsCall(method string, fn any) nexus.Option {
	if method == "" {
		return nexus.Raw(di.Error(errors.New("peer.AsCall: method name is required")))
	}
	sh, err := nexus.InspectHandlerForExt(fn)
	if err != nil {
		return nexus.Raw(di.Error(fmt.Errorf("peer.AsCall(%q): %w", method, err)))
	}
	return sh.BuildInvokeOption(func(_ *nexus.App, bound nexus.BoundHandler) error {
		entry := &callEntry{
			Method:   method,
			Bound:    bound,
			ArgsType: sh.ArgsType(),
			RetType:  sh.ReturnType(),
		}
		if _, dup := callTable.LoadOrStore(method, entry); dup {
			return fmt.Errorf("peer.AsCall(%q): method already registered", method)
		}
		return nil
	})
}

// Envelope is the wire format for /__peer/call. Same shape for
// request and response; Args carries inputs in, Result carries
// outputs out, Err is non-nil on application failure. One type
// means the codec is shared between client and server.
type Envelope struct {
	Method   string          `json:"method,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Err      *Error          `json:"error,omitempty"`
	Deadline int64           `json:"deadline_ns,omitempty"` // unix nanos; 0 = no deadline
}

// Error mirrors the framework's UserError on the wire so typed
// errors round-trip across peer hops. Fields beyond Code + Msg are
// opaque to the framework — they pass through verbatim.
type Error struct {
	Code    string         `json:"code"`
	Msg     string         `json:"msg"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Msg
	}
	return e.Code + ": " + e.Msg
}

// dispatchCall is the inbound handler bound to POST /__peer/call.
// Decodes the envelope, looks up the registered method, decodes
// args into the registered ArgsType, invokes the bound handler,
// marshals the result back.
//
// bus is the app's trace bus, threaded through from peer.Module's
// OnBoot. When non-nil, every inbound call publishes a peer.handle
// span parented to the caller's traceparent (if present) so the
// dashboard's waterfall stitches across binaries. nil bus = no
// trace events, but the dispatcher still functions normally.
//
// identity is the local app's identity, used for the span's
// service field so the dashboard renders "checkout-svc"
// (callee) under the caller's trace tree.
func dispatchCall(authMode AuthMode, secrets map[string]string, bus *trace.Bus, identity string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeEnvelope(w, http.StatusMethodNotAllowed, Envelope{
				Err: &Error{Code: "METHOD_NOT_ALLOWED", Msg: "POST only"},
			})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			writeEnvelope(w, http.StatusBadRequest, Envelope{
				Err: &Error{Code: "BAD_REQUEST", Msg: "read body: " + err.Error()},
			})
			return
		}
		if authMode == AuthHMAC {
			if err := verifyHMAC(r, body, secrets); err != nil {
				writeEnvelope(w, http.StatusUnauthorized, Envelope{
					Err: &Error{Code: "UNAUTHORIZED", Msg: err.Error()},
				})
				return
			}
		}
		var env Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			writeEnvelope(w, http.StatusBadRequest, Envelope{
				Err: &Error{Code: "BAD_REQUEST", Msg: "envelope decode: " + err.Error()},
			})
			return
		}
		entryAny, ok := callTable.Load(env.Method)
		if !ok {
			writeEnvelope(w, http.StatusNotFound, Envelope{
				Err: &Error{Code: "NOT_FOUND", Msg: "method " + env.Method + " not registered"},
			})
			return
		}
		entry := entryAny.(*callEntry)

		ctx := r.Context()
		if env.Deadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, time.Unix(0, env.Deadline))
			defer cancel()
		}

		// Trace stitching. Bus must be installed FIRST so any
		// span emitted downstream sees it; then the synthetic
		// remote parent is installed off the inbound
		// traceparent header (no-op when the header is absent
		// or malformed — local span becomes a fresh root); then
		// the peer.handle span attaches to that parent.
		ctx = trace.WithBus(ctx, bus)
		callerPeer := r.Header.Get("X-Nexus-Peer")
		if tid, pid, ok := trace.ParseTraceparent(r.Header.Get("traceparent")); ok {
			ctx = trace.WithRemoteParent(ctx, tid, pid, callerPeer, env.Method)
		}
		var span *trace.Span
		ctx, span = trace.StartSpan(ctx, "peer.handle "+env.Method,
			trace.Str("peer.caller", callerPeer),
			trace.Str("peer.method", env.Method),
			trace.Str("peer.local", identity))

		// Build args. The handler may declare no args (callTable
		// entry.ArgsType == nil), a flat-args struct, or a
		// Params[T] — Bound abstracts all three uniformly,
		// accepting `nil` when there's no args struct.
		var argsVal any
		if entry.ArgsType != nil {
			argsPtr := reflect.New(entry.ArgsType)
			if len(env.Args) > 0 {
				if err := json.Unmarshal(env.Args, argsPtr.Interface()); err != nil {
					span.End(err)
					writeEnvelope(w, http.StatusBadRequest, Envelope{
						Err: &Error{Code: "BAD_ARGS", Msg: err.Error()},
					})
					return
				}
			}
			argsVal = argsPtr.Elem().Interface()
		}

		result, hErr := entry.Bound(ctx, argsVal)
		span.End(hErr)
		if hErr != nil {
			writeEnvelope(w, http.StatusOK, Envelope{Err: errorToWire(hErr)})
			return
		}
		if result == nil {
			writeEnvelope(w, http.StatusOK, Envelope{})
			return
		}
		raw, mErr := json.Marshal(result)
		if mErr != nil {
			writeEnvelope(w, http.StatusInternalServerError, Envelope{
				Err: &Error{Code: "MARSHAL", Msg: mErr.Error()},
			})
			return
		}
		writeEnvelope(w, http.StatusOK, Envelope{Result: raw})
	}
}

// maxBodyBytes caps inbound envelope size. 4 MiB is generous for
// typical RPC payloads and tight enough that a malicious peer can't
// exhaust memory by streaming an unbounded body.
const maxBodyBytes = 4 << 20

func writeEnvelope(w http.ResponseWriter, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// errorToWire normalizes any Go error into the wire Error type. If
// the source is already a *Error (round-tripping through multiple
// hops) it's returned verbatim; otherwise we wrap the message and
// tag it INTERNAL so callers can distinguish framework errors from
// application typed errors.
func errorToWire(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Code: "INTERNAL", Msg: err.Error()}
}

// verifyHMAC checks the Authorization header against the body hash
// using the shared secret for the peer named in the X-Nexus-Peer
// header. Format: `Nexus-HMAC <peer>:<unix-seconds>:<hex-sig>` where
// sig = HMAC-SHA256(secret, peer + ":" + ts + ":" + sha256(body)).
// Replay window is ±30s of clock skew.
func verifyHMAC(r *http.Request, body []byte, secrets map[string]string) error {
	authHdr := r.Header.Get("Authorization")
	const prefix = "Nexus-HMAC "
	if !strings.HasPrefix(authHdr, prefix) {
		return errors.New("missing or malformed Nexus-HMAC header")
	}
	parts := strings.SplitN(authHdr[len(prefix):], ":", 3)
	if len(parts) != 3 {
		return errors.New("Nexus-HMAC header: want <peer>:<ts>:<sig>")
	}
	peerName, tsStr, sigHex := parts[0], parts[1], parts[2]
	secret, ok := secrets[peerName]
	if !ok {
		return fmt.Errorf("no HMAC secret configured for peer %q", peerName)
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("Nexus-HMAC ts: not an integer")
	}
	if skew := time.Since(time.Unix(ts, 0)); skew < -30*time.Second || skew > 30*time.Second {
		return fmt.Errorf("Nexus-HMAC ts: clock skew %s exceeds 30s window", skew)
	}
	bodyHash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%d:%s", peerName, ts, hex.EncodeToString(bodyHash[:]))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sigHex)) {
		return errors.New("Nexus-HMAC signature mismatch")
	}
	return nil
}
