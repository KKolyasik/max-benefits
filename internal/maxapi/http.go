package maxapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
)

const maxRetries429 = 3

// retryAfter429 is the base pause before retrying a rate-limited request when
// the server does not say how long to wait. A variable for tests.
var retryAfter429 = time.Second

// newHTTPClient builds the client used by the SDK. It trusts the system
// roots plus caFile (if set) and transparently retries "429 Too Many
// Requests" responses.
func newHTTPClient(caFile string, timeout time.Duration, log *slog.Logger) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 32
	if caFile != "" {
		pool, err := certPool(caFile)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &retry429{next: tr, log: log},
	}, nil
}

// certPool returns the system roots plus the certificate from path, which
// may be PEM or DER encoded.
func certPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if block, _ := pem.Decode(data); block == nil {
		cert, err := x509.ParseCertificate(data)
		if err != nil {
			return nil, fmt.Errorf("CA file %s is neither PEM nor DER: %w", path, err)
		}
		pool.AddCert(cert)
		return pool, nil
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no certificates found in CA file %s", path)
	}
	return pool, nil
}

type retry429 struct {
	next http.RoundTripper
	log  *slog.Logger
}

func (t *retry429) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := t.next.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || attempt > maxRetries429 {
			return resp, err
		}
		// Only replay requests whose body can be rewound.
		if req.Body != nil && req.GetBody == nil {
			return resp, nil
		}
		wait := retryAfter429 * time.Duration(attempt)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		_ = resp.Body.Close()
		t.log.Warn("max api rate limit hit, retrying", "path", req.URL.Path, "wait", wait)

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
}
