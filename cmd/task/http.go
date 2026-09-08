package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// httpRequest performs one outbound request from inside the sandbox.
//
// It is the only tool that asks for the network, and it exists as much to make
// the NetworkPolicy observable as to fetch anything: every other task pod runs
// with egress denied, so nothing else would notice if the policy stopped being
// enforced.
//
// The address checks below duplicate what the NetworkPolicy already does. That
// is not redundancy for its own sake — it is the same two-mechanism argument as
// the task ServiceAccount, which is both bound to nothing AND not mounted. The
// CNI policy is the control that holds when this code is wrong; this code is
// the control that holds when the cluster's CNI turns out not to enforce policy
// at all, which is the default on kind and is exactly the failure this project
// went out of its way to avoid.
func httpRequest(params map[string]any, stderr io.Writer) (any, int) {
	raw, _ := params["url"].(string)
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		fmt.Fprintf(stderr, "task: http_request: url %q is not an absolute URL\n", raw)
		return nil, 2
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		fmt.Fprintf(stderr, "task: http_request: scheme %q is not http or https\n", u.Scheme)
		return nil, 2
	}
	if u.User != nil {
		fmt.Fprintln(stderr, "task: http_request: credentials in the URL are refused")
		return nil, 2
	}

	method := strings.ToUpper(stringParam(params, "method"))
	if method == "" {
		method = http.MethodGet
	}

	maxBytes := intParam(params, "max_bytes")
	if maxBytes <= 0 || maxBytes > defaultMaxBody {
		maxBytes = defaultMaxBody
	}

	var body io.Reader
	if s := stringParam(params, "body"); s != "" {
		body = strings.NewReader(s)
	}

	// A context, not just the client timeout, so the deadline covers the whole
	// exchange including reading the body. A slow-drip response is the shape a
	// hostile endpoint uses to hold a worker open, and the client's own Timeout
	// field is the one that does cover it — belt and braces, and the context is
	// what a future cancellation would hook into.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		fmt.Fprintf(stderr, "task: http_request: %v\n", err)
		return nil, 2
	}
	req.Header.Set("User-Agent", userAgent)
	if headers, ok := params["headers"].(map[string]any); ok {
		for k, v := range headers {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.EqualFold(k, "host") {
				// Rejected at submit time too. Here because a request that
				// reaches an allowed address and then names a different
				// virtual host is how an IP-based control gets walked past.
				fmt.Fprintln(stderr, "task: http_request: the Host header may not be set")
				return nil, 2
			}
			req.Header.Set(k, s)
		}
	}

	resp, err := guardedClient().Do(req)
	if err != nil {
		// Exit 1, not 2: a network failure is the endpoint's problem or the
		// policy's, and both are worth another attempt. Exit 2 is reserved for
		// a broken contract, which no retry fixes.
		fmt.Fprintf(stderr, "task: http_request: %v\n", err)
		return nil, 1
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the cap, so a body that exactly fills it can be told from
	// one that was cut off — and the caller is told which.
	read, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		fmt.Fprintf(stderr, "task: http_request: reading the body: %v\n", err)
		return nil, 1
	}
	truncated := len(read) > maxBytes
	if truncated {
		read = read[:maxBytes]
	}

	out := map[string]any{
		"url":          resp.Request.URL.String(), // after redirects, not before
		"status":       resp.StatusCode,
		"headers":      flattenHeaders(resp.Header),
		"truncated":    truncated,
		"body_bytes":   len(read),
		"content_type": resp.Header.Get("Content-Type"),
	}
	// JSON cannot carry arbitrary bytes: marshalling invalid UTF-8 replaces it
	// with U+FFFD, silently, so a downstream step would receive corrupted data
	// that looks like text. Base64 for anything that is not valid UTF-8, with
	// the field name saying so.
	if utf8.Valid(read) {
		out["body"] = string(read)
	} else {
		out["body_base64"] = base64.StdEncoding.EncodeToString(read)
	}
	return out, 0
}

const (
	defaultMaxBody = 256 << 10
	requestTimeout = 30 * time.Second
	userAgent      = "RunMesh/1.0 (+https://github.com/kalanas210/runmesh)"
	maxRedirects   = 5
)

// guardedClient builds an HTTP client that refuses to connect to any address
// that is not on the public internet.
//
// The check lives in the dialer's Control hook, which runs after the address
// has been resolved and before connect(2) — and that placement is the whole
// point. Validating the hostname before dialling is defeated by DNS rebinding:
// the name resolves to a public address for the check and to 169.254.169.254
// for the connection. Checking the socket's actual destination cannot be
// rebound, and it covers every redirect hop and every retry for free, because
// each one opens a new connection through the same hook.
func guardedClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("refusing to dial %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("refusing to dial %q: not an IP address", address)
			}
			if !publicUnicast(ip) {
				return fmt.Errorf("refusing to connect to %s: only public addresses "+
					"are reachable from a task", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			// A task makes one request and exits. Pooling connections it will
			// never reuse just holds sockets open past the point the step is
			// done with them.
			DisableKeepAlives: true,
			MaxIdleConns:      1,
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// The scheme is re-checked because a redirect can change it, and
			// the address is re-checked by the dialer on the new connection.
			// Together they mean a 302 cannot walk out of the rules the first
			// request was held to.
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing to follow a redirect to scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// publicUnicast reports whether an address is one a task may reach.
//
// Deny-by-category rather than a CIDR list, so an address family or a special
// range nobody thought about does not fall through as "not on the list".
func publicUnicast(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), ip.IsUnspecified(),
		ip.IsPrivate(),          // RFC 1918 and RFC 4193
		ip.IsLinkLocalUnicast(), // 169.254.0.0/16 — the metadata service
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast():
		return false
	}
	// Carrier-grade NAT: not routable on the public internet, and used by some
	// cloud providers for internal services. net has no predicate for it.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return false
	}
	return ip.IsGlobalUnicast()
}

// flattenHeaders renders response headers as a single-valued map.
//
// Joining repeated values with ", " is what RFC 9110 says a recipient may do
// for every header except Set-Cookie — and Set-Cookie is dropped entirely a few
// lines down, because a cookie is a credential and a task's result is stored,
// logged and shown on a dashboard.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "set-cookie") {
			continue
		}
		out[k] = strings.Join(v, ", ")
	}
	return out
}

func stringParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

func intParam(params map[string]any, key string) int {
	switch v := params[key].(type) {
	case float64: // every JSON number decodes to float64
		return int(v)
	case string:
		return atoi(v)
	default:
		return 0
	}
}
