package redis

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is how to reach one Redis server. It carries no timeout of its own —
// deliberately, since internal/clock's purity test bans time.Now and
// context.WithTimeout outside package clock, and this package has to obey
// that like everything else under internal/. Every deadline a call to Do or
// Eval observes comes from the ctx the caller passed in, which is expected to
// arrive already bounded via clock.WithWriteDeadline, the same way every
// store call in internal/engine is.
type Config struct {
	Addr     string // host:port
	Username string // empty for legacy AUTH <password>; set for ACL AUTH <user> <password>
	Password string // empty disables AUTH entirely
	DB       int    // SELECTed once per connection, right after AUTH

	// PoolSize bounds how many connections may exist at once, idle or
	// checked out. Rate-limit checks happen on a worker goroutine holding a
	// pool capacity token (see internal/engine/observer.go's own doc on why
	// that matters), so serialising them all through one connection would
	// make this package the thing that shrinks the worker pool it is meant
	// to be a cheap check inside.
	PoolSize int
}

func (c *Config) setDefaults() {
	if c.PoolSize < 1 {
		c.PoolSize = 8
	}
}

// ParseURL parses redis://[[user]:password@]host[:port][/db]. The scheme
// rediss:// (TLS) is recognised and refused with a clear reason rather than
// silently connecting in plain text — see the package doc's reopening
// condition.
func ParseURL(raw string) (Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Config{}, fmt.Errorf("redis: %q is not a URL: %w", raw, err)
	}
	switch u.Scheme {
	case "redis":
	case "rediss":
		return Config{}, fmt.Errorf("redis: %q uses rediss:// (TLS), which this "+
			"hand-written client does not speak yet; use redis:// to a host that "+
			"terminates TLS itself, or see ADR 0015's reopening condition", raw)
	default:
		return Config{}, fmt.Errorf("redis: %q must use redis://, got scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("redis: %q names no host", raw)
	}
	cfg := Config{Addr: u.Host}
	if !strings.Contains(cfg.Addr, ":") {
		cfg.Addr += ":6379"
	}
	if u.User != nil {
		cfg.Username = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		db, err := strconv.Atoi(p)
		if err != nil {
			return Config{}, fmt.Errorf("redis: %q is not a database index in %q", p, raw)
		}
		cfg.DB = db
	}
	return cfg, nil
}

// conn is one authenticated, SELECTed TCP connection.
type conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

// dial opens and prepares one connection. It honours ctx for both the TCP
// handshake (net.Dialer.DialContext already does this without this package
// naming a timeout of its own) and for AUTH/SELECT, which run under whatever
// deadline ctx.Deadline() gives conn.do below.
func dial(ctx context.Context, cfg Config) (*conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("redis: dial %s: %w", cfg.Addr, err)
	}
	c := &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}
	if err := c.applyDeadline(ctx); err != nil {
		_ = nc.Close()
		return nil, err
	}

	if cfg.Password != "" {
		args := []string{"AUTH"}
		if cfg.Username != "" {
			args = append(args, cfg.Username)
		}
		args = append(args, cfg.Password)
		if v, err := c.do(args...); err != nil || v.Kind == KindError {
			_ = nc.Close()
			return nil, authOrSelectError("AUTH", v, err)
		}
	}
	if cfg.DB != 0 {
		if v, err := c.do("SELECT", strconv.Itoa(cfg.DB)); err != nil || v.Kind == KindError {
			_ = nc.Close()
			return nil, authOrSelectError("SELECT", v, err)
		}
	}
	return c, nil
}

func authOrSelectError(step string, v Value, err error) error {
	if err != nil {
		return fmt.Errorf("redis: %s: %w", step, err)
	}
	return fmt.Errorf("redis: %s: %w", step, v.Err)
}

// applyDeadline mirrors ctx onto the socket. A ctx with no deadline clears
// whatever the socket had, rather than leaving a stale one from a previous
// call in place — SetDeadline(time.Time{}) is the documented way to do that,
// and it is a value, not a call to time.Now.
func (c *conn) applyDeadline(ctx context.Context) error {
	d, ok := ctx.Deadline()
	if !ok {
		return c.nc.SetDeadline(zeroTime)
	}
	return c.nc.SetDeadline(d)
}

func (c *conn) do(args ...string) (Value, error) {
	if _, err := c.w.Write(encodeCommand(args...)); err != nil {
		return Value{}, fmt.Errorf("redis: write: %w", err)
	}
	if err := c.w.Flush(); err != nil {
		return Value{}, fmt.Errorf("redis: flush: %w", err)
	}
	return decodeValue(c.r)
}

// Client is a small connection pool over one Redis server: borrow a conn,
// use it, return it — or close it and free its slot if it errored. There is
// no background health-check goroutine (the engine's own package doc is
// exactly why: a goroutine here is a goroutine internal/engine did not spawn
// and cannot account for in its own census); a broken connection is
// discovered on next use and replaced lazily.
type Client struct {
	cfg  Config
	idle chan *conn
	// sem bounds total connections ALIVE at once, idle or checked out — idle's
	// own length cannot say that, since a borrowed conn is not in it.
	sem    chan struct{}
	closed atomic.Bool
}

// New returns a Client. It dials nothing yet; the first Do or Eval does.
func New(cfg Config) *Client {
	cfg.setDefaults()
	return &Client{
		cfg:  cfg,
		idle: make(chan *conn, cfg.PoolSize),
		sem:  make(chan struct{}, cfg.PoolSize),
	}
}

func (cl *Client) borrow(ctx context.Context) (*conn, error) {
	select {
	case c := <-cl.idle:
		return c, nil
	default:
	}
	select {
	case cl.sem <- struct{}{}:
		c, err := dial(ctx, cl.cfg)
		if err != nil {
			<-cl.sem // the reservation was never spent; give it back
			return nil, err
		}
		return c, nil
	case c := <-cl.idle:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release returns c to the pool, or closes it and frees its slot when it
// errored or the pool has been closed underneath it.
func (cl *Client) release(c *conn, err error) {
	if err == nil && !cl.closed.Load() {
		select {
		case cl.idle <- c:
			return
		default:
			// idle is already at PoolSize, which sem's own capacity says
			// cannot happen — closing rather than panicking on a channel
			// invariant this package itself is responsible for keeping.
		}
	}
	_ = c.nc.Close()
	<-cl.sem
}

// Do sends one command and returns its decoded reply. A server error reply
// (Value.Kind == KindError) is also returned as the error, so a caller that
// only checks err never has to remember to check Kind too.
func (cl *Client) Do(ctx context.Context, args ...string) (Value, error) {
	c, err := cl.borrow(ctx)
	if err != nil {
		return Value{}, err
	}
	if err := c.applyDeadline(ctx); err != nil {
		cl.release(c, err)
		return Value{}, err
	}
	v, err := c.do(args...)
	cl.release(c, err)
	if err != nil {
		return Value{}, err
	}
	if v.Kind == KindError {
		return v, v.Err
	}
	return v, nil
}

// Eval runs a Lua script with EVAL, always inline rather than via EVALSHA and
// SCRIPT LOAD. That is a deliberate simplification, not an oversight: it
// costs re-sending a few hundred bytes of script text on every call, against
// implementing SCRIPT LOAD, caching the SHA1 this client would then have to
// compute itself (Redis returns it, but a client MUST also be able to derive
// it to pre-load after a reconnect), and handling NOSCRIPT on a cache miss —
// real protocol surface for a control-plane check that runs once per attempt,
// not once per request a fleet serves. See ADR 0015.
func (cl *Client) Eval(ctx context.Context, script string, keys []string, args ...string) (Value, error) {
	cmd := make([]string, 0, 3+len(keys)+len(args))
	cmd = append(cmd, "EVAL", script, strconv.Itoa(len(keys)))
	cmd = append(cmd, keys...)
	cmd = append(cmd, args...)
	return cl.Do(ctx, cmd...)
}

// Ping is a connectivity check, and how the caller in cmd/server decides
// whether a configured RUNMESH_REDIS_URL is actually reachable at boot.
func (cl *Client) Ping(ctx context.Context) error {
	v, err := cl.Do(ctx, "PING")
	if err != nil {
		return err
	}
	if v.Kind != KindStatus || v.Str != "PONG" {
		return fmt.Errorf("redis: PING replied %v, want PONG", v)
	}
	return nil
}

// Close closes every idle connection and marks the pool closed, so a
// connection currently checked out is closed by its own release rather than
// returned to a pool nothing will ever drain again. It never closes the idle
// channel itself: a concurrent release sending to it after Close has started
// must not panic.
func (cl *Client) Close() error {
	cl.closed.Store(true)
	var mu sync.Mutex
	var firstErr error
	for {
		select {
		case c := <-cl.idle:
			if err := c.nc.Close(); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
			select {
			case <-cl.sem:
			default:
			}
		default:
			return firstErr
		}
	}
}

// zeroTime clears a socket deadline: net.Conn.SetDeadline's documented way to
// remove one. A named zero value, not a call — time.Time{} is a composite
// literal of the TYPE time.Time, which internal/clock's purity test does not
// and should not ban; only the nine identifiers it lists (time.Now among
// them) are.
var zeroTime time.Time
