// Package fetch downloads public regulatory feeds and catalog targets with
// conditional ETag support, SSRF protection and bounded scratch-file buffering
// so Tidegauge stays within its 30 MiB memory budget.
package fetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

const (
	defaultTimeout  = 20 * time.Second
	defaultMaxBytes = 16 << 20 // 16 MiB cap matching tools/ecfr
	inMemoryLimit   = 64 << 10 // spill bodies >64 KiB to ScratchDir
	maxRedirects    = 5
	userAgent       = "ClearedStack-Tidegauge/0.1"
)

// Options configures a Fetcher.
type Options struct {
	ScratchDir          string
	Timeout             time.Duration
	MaxBytes            int
	HTTPClient          *http.Client
	AllowPrivateForTest bool
}

// Fetcher performs bounded, sanitized HTTP GET requests.
type Fetcher struct {
	scratchDir   string
	maxBytes     int
	allowPrivate bool
	client       *http.Client
}

// Response is one fetched HTTP snapshot. Callers must always call Close when
// err == nil to remove any temporary scratch file.
type Response struct {
	StatusCode  int
	NotModified bool
	ETag        string
	Origin      string
	Body        io.ReadSeeker
	cleanup     func() error
}

// Close releases any temporary file backing Body.
func (r *Response) Close() error {
	if r == nil || r.cleanup == nil {
		return nil
	}
	fn := r.cleanup
	r.cleanup = nil
	return fn()
}

// New creates a Fetcher with SSRF-safe public-IP dialing enabled unless
// opts.AllowPrivateForTest is explicitly set by a unit test.
func New(opts Options) *Fetcher {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	f := &Fetcher{
		scratchDir:   opts.ScratchDir,
		maxBytes:     maxBytes,
		allowPrivate: opts.AllowPrivateForTest,
	}

	var base *http.Client
	if opts.HTTPClient != nil {
		copied := *opts.HTTPClient
		base = &copied
	} else {
		base = &http.Client{Timeout: timeout}
	}
	if !f.allowPrivate && base.Transport == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
		base.Transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           safeDialContext(dialer),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
		}
	}
	base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("too many redirects")
		}
		if werr := validateTargetURL(req.URL, f.allowPrivate); werr != nil {
			return fmt.Errorf("redirect blocked")
		}
		return nil
	}
	f.client = base
	return f
}

// Get fetches rawURL, sending If-None-Match when prevETag is non-empty.
// Returned *watch.Error messages never contain URLs, hostnames or IP addresses.
func (f *Fetcher) Get(ctx context.Context, rawURL string, prevETag string) (*Response, *watch.Error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "target URL is malformed"}
	}
	if werr := validateTargetURL(u, f.allowPrivate); werr != nil {
		return nil, werr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &watch.Error{Code: watch.CodeParse, Message: "target request could not be constructed"}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/xml, application/atom+xml, application/rss+xml, application/json, text/html;q=0.9, */*;q=0.5")
	if strings.TrimSpace(prevETag) != "" {
		req.Header.Set("If-None-Match", strings.TrimSpace(prevETag))
	}

	resp, err := f.client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "redirect blocked") || strings.Contains(err.Error(), "disallowed IP") {
			return nil, &watch.Error{Code: watch.CodeParse, Message: "target redirected to a disallowed address"}
		}
		if watch.IsTimeout(err) || ctx.Err() != nil {
			return nil, &watch.Error{Code: watch.CodeTimeout, Message: "upstream target did not respond in time"}
		}
		return nil, &watch.Error{Code: watch.CodeTimeout, Message: "upstream target could not be reached"}
	}
	defer resp.Body.Close()

	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" && resp.StatusCode == http.StatusNotModified {
		etag = strings.TrimSpace(prevETag)
	}
	origin := ""
	if resp.Request != nil && resp.Request.URL != nil {
		origin = resp.Request.URL.Scheme + "://" + strings.ToLower(resp.Request.URL.Host)
	} else {
		origin = u.Scheme + "://" + strings.ToLower(u.Host)
	}

	if resp.StatusCode == http.StatusNotModified {
		return &Response{
			StatusCode:  http.StatusNotModified,
			NotModified: true,
			ETag:        etag,
			Origin:      origin,
			Body:        bytes.NewReader(nil),
		}, nil
	}

	bodyReader, cleanup, werr := f.bufferBody(resp.Body)
	if werr != nil {
		return nil, werr
	}
	return &Response{
		StatusCode: resp.StatusCode,
		ETag:       etag,
		Origin:     origin,
		Body:       bodyReader,
		cleanup:    cleanup,
	}, nil
}

func (f *Fetcher) bufferBody(r io.Reader) (io.ReadSeeker, func() error, *watch.Error) {
	head := make([]byte, inMemoryLimit+1)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		if watch.IsTimeout(err) {
			return nil, nil, &watch.Error{Code: watch.CodeTimeout, Message: "reading upstream response timed out"}
		}
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "upstream response body could not be read"}
	}
	if n > f.maxBytes {
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "upstream response exceeds size limit"}
	}
	if n <= inMemoryLimit {
		buf := make([]byte, n)
		copy(buf, head[:n])
		return bytes.NewReader(buf), nil, nil
	}

	// Spill larger payloads into ScratchDir so heap usage stays small.
	tmpFile, ferr := os.CreateTemp(f.scratchDir, "fetch-*.tmp")
	if ferr != nil {
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "scratch file could not be created"}
	}
	tmpPath := tmpFile.Name()
	abort := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	if _, werr := tmpFile.Write(head[:n]); werr != nil {
		abort()
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "scratch file write failed"}
	}
	remainingLimit := int64(f.maxBytes - n + 1)
	written, cerr := io.Copy(tmpFile, io.LimitReader(r, remainingLimit))
	if cerr != nil {
		abort()
		if watch.IsTimeout(cerr) {
			return nil, nil, &watch.Error{Code: watch.CodeTimeout, Message: "reading upstream response timed out"}
		}
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "upstream response body could not be read"}
	}
	if int64(n)+written > int64(f.maxBytes) {
		abort()
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "upstream response exceeds size limit"}
	}
	if _, serr := tmpFile.Seek(0, io.SeekStart); serr != nil {
		abort()
		return nil, nil, &watch.Error{Code: watch.CodeParse, Message: "scratch file could not be rewound"}
	}
	cleanup := func() error {
		err1 := tmpFile.Close()
		err2 := os.Remove(tmpPath)
		if err1 != nil {
			return err1
		}
		return err2
	}
	return tmpFile, cleanup, nil
}

func validateTargetURL(u *url.URL, allowPrivate bool) *watch.Error {
	if u == nil || u.Host == "" {
		return &watch.Error{Code: watch.CodeParse, Message: "target URL has no host"}
	}
	if u.User != nil {
		return &watch.Error{Code: watch.CodeParse, Message: "target URL must not contain credentials"}
	}
	if allowPrivate {
		if u.Scheme != "https" && u.Scheme != "http" {
			return &watch.Error{Code: watch.CodeParse, Message: "target URL scheme is not allowed"}
		}
		return nil
	}
	if u.Scheme != "https" {
		return &watch.Error{Code: watch.CodeParse, Message: "target URL must use HTTPS"}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || host == "localhost" {
		return &watch.Error{Code: watch.CodeParse, Message: "target host is not a public domain"}
	}
	for _, suffix := range []string{".local", ".localhost", ".internal", ".cluster.local", ".svc"} {
		if strings.HasSuffix(host, suffix) {
			return &watch.Error{Code: watch.CodeParse, Message: "target host is not a public domain"}
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddr(addr) {
			return &watch.Error{Code: watch.CodeParse, Message: "target IP address is not public"}
		}
	}
	return nil
}

func safeDialContext(dialer *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no IP addresses resolved")
		}
		for _, ip := range ips {
			if !isPublicAddr(ip.Unmap()) {
				return nil, fmt.Errorf("disallowed IP")
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, lastErr
	}
}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
var benchmarkPrefix = netip.MustParsePrefix("198.18.0.0/15")

func isPublicAddr(addr netip.Addr) bool {
	a := addr.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() ||
		a.IsMulticast() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() {
		return false
	}
	if cgnatPrefix.Contains(a) || benchmarkPrefix.Contains(a) {
		return false
	}
	return true
}
