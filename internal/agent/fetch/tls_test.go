package fetch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCert(t *testing.T, tmpl *x509.Certificate, parent *testCA) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

func ca(name string) *x509.Certificate {
	return &x509.Certificate{
		Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
}

// A server that sends only its own certificate, like metro.spb.ru, is
// accepted once the intermediate is fetched from the address in the
// certificate, and only if the chain ends at a trusted root.
func TestMissingIntermediateIsFetched(t *testing.T) {
	root := newCert(t, ca("Test Root"), nil)
	sub := newCert(t, ca("Test Sub CA"), root)

	aia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sub.cert.Raw)
	}))
	defer aia.Close()

	leaf := newCert(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "site.test"},
		DNSNames:              []string{"site.test"},
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		IssuingCertificateURL: []string{aia.URL + "/sub.crt"},
	}, sub)

	site := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	// Only the leaf, no intermediate.
	site.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}}}
	site.StartTLS()
	defer site.Close()

	trusted := x509.NewCertPool()
	trusted.AddCert(root.cert)
	resp, err := clientFor(site, trusted).Get("https://site.test/")
	if err != nil {
		t.Fatalf("the chain must verify with the fetched intermediate: %v", err)
	}
	_ = resp.Body.Close()

	other := newCert(t, ca("Other Root"), nil)
	untrusted := x509.NewCertPool()
	untrusted.AddCert(other.cert)
	if !refused(clientFor(site, untrusted), "https://site.test/") {
		t.Fatal("a chain to an untrusted root must be rejected")
	}
}

func TestWrongHostIsRejected(t *testing.T) {
	root := newCert(t, ca("Test Root"), nil)
	leaf := newCert(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "other.test"},
		DNSNames:    []string{"other.test"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	}, root)
	site := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	site.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}}}
	site.StartTLS()
	defer site.Close()

	trusted := x509.NewCertPool()
	trusted.AddCert(root.cert)
	if !refused(clientFor(site, trusted), "https://site.test/") {
		t.Fatal("a certificate for another host must be rejected")
	}
	if !refused(NewClient(trusted, 5*time.Second), site.URL) {
		t.Fatal("an IP address has no host name to check and must be refused")
	}
}

// clientFor sends requests for any host to the test server, as if its name
// resolved there.
func clientFor(site *httptest.Server, roots *x509.CertPool) *http.Client {
	c := NewClient(roots, 5*time.Second)
	c.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
	}
	return c
}

func refused(c *http.Client, url string) bool {
	resp, err := c.Get(url)
	if err != nil {
		return true
	}
	_ = resp.Body.Close()
	return false
}
