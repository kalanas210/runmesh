package httpapi

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

type ctxKey int

const ctxRequestInfo ctxKey = iota

// reqInfo is per-request scratch space that inner layers fill in and the
// outermost logger reads back.
//
// A pointer rather than separate context values, and this is not incidental.
// Middleware that adds a context value has to pass a NEW *http.Request
// downstream, and the outer layers keep the old one — so anything an inner
// layer learned would be invisible to the log line, which is the one place it
// is actually needed. One pointer installed at the top, mutated in place on
// the way down, read on the way out.
//
// It is written and read on a single goroutine, sequenced by the call stack.
type reqInfo struct {
	id     string
	keyID  string
	key    config.APIKey
	hasKey bool
	route  string
}

func infoFrom(ctx context.Context) *reqInfo {
	info, _ := ctx.Value(ctxRequestInfo).(*reqInfo)
	return info
}

// RequestIDFrom returns the request id attached by the RequestID middleware.
func RequestIDFrom(ctx context.Context) string {
	if info := infoFrom(ctx); info != nil {
		return info.id
	}
	return ""
}

// APIKeyIDFrom returns the id of the API key that authenticated the request.
// It is the key's NAME, never the key itself.
func APIKeyIDFrom(ctx context.Context) string {
	if info := infoFrom(ctx); info != nil {
		return info.keyID
	}
	return ""
}

// APIKeyFrom returns the key that authenticated the request, so a route can ask
// what it is allowed to do. It reports false on an unauthenticated request —
// which, for a scoped route, means Auth never ran, and refusing is the only
// safe answer.
func APIKeyFrom(ctx context.Context) (config.APIKey, bool) {
	if info := infoFrom(ctx); info != nil && info.hasKey {
		return info.key, true
	}
	return config.APIKey{}, false
}

// middleware is the standard net/http shape. There is no framework here and no
// custom context type: a middleware is a function from handler to handler, and
// composing them is a fold.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the FIRST argument is outermost.
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// statusRecorder captures what was actually written, so the log line reports
// the real status rather than assuming 200.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	written  int64
	panicked bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// RequestID is the OUTERMOST middleware, so every other layer — including the
// panic handler — has an id to correlate with.
//
// A client-supplied X-Request-ID is echoed only if it is short and printable.
// Hostile values (control characters, a 10 KB header) are replaced rather than
// propagated, because this string ends up in log lines and in a JSON body.
func RequestID(clk clock.Clock) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitiseRequestID(r.Header.Get("X-Request-ID"))
			if id == "" {
				id = runmesh.NewID("req_", clk.Now())
			}
			w.Header().Set("X-Request-ID", id)
			info := &reqInfo{id: id, route: "unmatched"}
			ctx := context.WithValue(r.Context(), ctxRequestInfo, info)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func sanitiseRequestID(v string) string {
	if v == "" || len(v) > 128 {
		return ""
	}
	for _, c := range v {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return v
}

// Logger emits one structured line per request.
//
// It wraps Recover rather than the other way round, deliberately: a panic must
// be caught, turned into a 500, and THEN observed by this deferred log line,
// so the record says status=500. Reversed, the log would record status=0, and
// the most interesting requests would be the least legible.
func Logger(log *slog.Logger, clk clock.Clock) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := clk.Now()
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				level := slog.LevelInfo
				switch {
				case rec.status >= 500:
					level = slog.LevelError
				case rec.status >= 400:
					level = slog.LevelWarn
				}
				log.LogAttrs(r.Context(), level, "http request",
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("method", r.Method),
					// The matched ROUTE, not the raw path, so this label stays
					// low-cardinality and is safe to use as a metric dimension
					// in Week 3. CaptureRoute records it from inside the chain.
					slog.String("route", routeOf(r.Context())),
					slog.String("path", r.URL.Path),
					slog.Int("status", rec.status),
					slog.Int64("bytes", rec.written),
					slog.Bool("panic", rec.panicked),
					slog.String("api_key_id", APIKeyIDFrom(r.Context())),
					slog.Int64("duration_ms", clk.Since(start).Milliseconds()),
				)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// Recover turns a handler panic into a well-formed 500 carrying the usual
// envelope. The connection is not dropped and the process survives: one bad
// request must not take the runtime down.
func Recover(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if sr, ok := w.(*statusRecorder); ok {
					sr.panicked = true
				}
				log.Error("handler panic",
					"request_id", RequestIDFrom(r.Context()),
					"path", r.URL.Path, "panic", p)
				writeJSON(w, log, http.StatusInternalServerError, errorEnvelope{Error: APIError{
					Code:      CodeInternal,
					Message:   "internal error",
					RequestID: RequestIDFrom(r.Context()),
				}})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimit caps the request body. MaxBytesReader makes the limit the reader's
// problem rather than a Content-Length check a chunked request can lie about.
func BodyLimit(max int64) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Auth accepts `Authorization: Bearer <key>`.
//
// The lookup key is a sha256 digest, so the map lookup itself carries no
// timing information about how many leading bytes of a guessed KEY matched —
// the property a naive string comparison lacks. The constant-time compare
// afterwards guards the digest itself.
//
// health and ready are exempt: a load balancer must be able to probe a service
// without holding a credential.
func Auth(keys map[[32]byte]config.APIKey, log *slog.Logger, exempt func(*http.Request) bool) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			presented, ok := bearerToken(r)
			if !ok {
				writeError(w, r, log, errUnauthenticated)
				return
			}
			digest := config.KeyDigest(presented)
			key, found := keys[digest]
			if !found {
				writeError(w, r, log, errUnauthenticated)
				return
			}
			want := config.KeyDigest(presented)
			if subtle.ConstantTimeCompare(digest[:], want[:]) != 1 {
				writeError(w, r, log, errUnauthenticated)
				return
			}
			// Recorded in place rather than in a new context, so the outermost
			// log line — which holds the original request — can still see it,
			// and so the scope check inside the mux can read it back without a
			// second lookup.
			if info := infoFrom(r.Context()); info != nil {
				info.keyID = key.ID
				info.key, info.hasKey = key, true
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CaptureRoute records the matched route pattern for the log line.
//
// It has to be the INNERMOST middleware, wrapping the mux directly, because
// ServeMux sets Pattern on the request as it dispatches — and every layer
// above has its own older copy. Reading it here, on the request the mux
// actually received, is what turns "unmatched" into "GET /api/v1/jobs/{id}".
//
// If a future ServeMux stops populating Pattern, the label degrades to
// "unmatched" rather than breaking anything.
func CaptureRoute() middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := infoFrom(r.Context())
			if info != nil {
				defer func() {
					if r.Pattern != "" {
						info.route = r.Pattern
					}
				}()
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func routeOf(ctx context.Context) string {
	if info := infoFrom(ctx); info != nil && info.route != "" {
		return info.route
	}
	return "unmatched"
}
