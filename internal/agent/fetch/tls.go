package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NewClient returns an HTTP client for reading sites. It trusts the given
// roots and, like browsers, fetches an intermediate certificate a server
// doesn't send: metro.spb.ru and many other official sites send only their
// own certificate, and Go's standard verification rejects them.
func NewClient(roots *x509.CertPool, timeout time.Duration) *http.Client {
	v := &verifier{roots: roots, issuers: map[string]*x509.Certificate{}, http: &http.Client{Timeout: 10 * time.Second}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The chain is verified in VerifyConnection instead, which does all
		// the standard checks and only adds the missing intermediate.
		InsecureSkipVerify: true,
		VerifyConnection:   v.verify,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

type verifier struct {
	roots   *x509.CertPool
	http    *http.Client
	mu      sync.Mutex
	issuers map[string]*x509.Certificate
}

func (v *verifier) verify(cs tls.ConnectionState) error {
	// ServerName is empty for an IP address; found pages are addressed by
	// host names, so such connections are simply refused.
	if cs.ServerName == "" || len(cs.PeerCertificates) == 0 {
		return errors.New("tls: only host names with a certificate are supported")
	}
	leaf := cs.PeerCertificates[0]
	opts := x509.VerifyOptions{Roots: v.roots, DNSName: cs.ServerName, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := leaf.Verify(opts)
	var unknown x509.UnknownAuthorityError
	if err == nil || !errors.As(err, &unknown) {
		return err
	}
	// The fetched certificate is not trusted by itself: the chain still has
	// to end at one of the roots.
	for _, u := range leaf.IssuingCertificateURL {
		issuer := v.issuer(u)
		if issuer == nil {
			continue
		}
		opts.Intermediates.AddCert(issuer)
		if _, retry := leaf.Verify(opts); retry == nil {
			return nil
		}
	}
	return err
}

// issuer downloads a CA certificate by its URL from a certificate, once.
func (v *verifier) issuer(url string) *x509.Certificate {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.issuers[url]; ok {
		return c
	}
	c := v.download(url)
	v.issuers[url] = c
	return c
}

func (v *verifier) download(url string) *x509.Certificate {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	if block, _ := pem.Decode(data); block != nil {
		data = block.Bytes
	}
	c, err := x509.ParseCertificate(data)
	if err != nil || !c.IsCA {
		return nil
	}
	return c
}
