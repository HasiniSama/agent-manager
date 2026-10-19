// Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package scorewriter

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
)

// testPKI is a CA and a server certificate for 127.0.0.1 written to a temp dir.
type testPKI struct {
	certFile, keyFile, caFile string
	roots                     *x509.CertPool
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "score writer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p := testPKI{
		certFile: filepath.Join(dir, "tls.crt"),
		keyFile:  filepath.Join(dir, "tls.key"),
		caFile:   filepath.Join(dir, "ca.crt"),
		roots:    x509.NewCertPool(),
	}
	p.roots.AddCert(caCert)
	writePEM(t, p.certFile, "CERTIFICATE", der)
	writePEM(t, p.keyFile, "EC PRIVATE KEY", keyDER)
	writePEM(t, p.caFile, "CERTIFICATE", caDER)
	return p
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testConfig points at the test PKI and a database port nothing listens on.
func testConfig(t *testing.T, p testPKI) *config.ScoreWriterConfig {
	t.Helper()
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &config.ScoreWriterConfig{
		PublishPort:  9099,
		InternalPort: 9199,
		TLS:          config.TLSFiles{CertFile: p.certFile, KeyFile: p.keyFile},
		Database: config.ScoreDatabaseConfig{
			Host: "127.0.0.1", Port: closedPort(t), Name: "amp_scores", User: "amp_scores_writer",
			PasswordFile: password, CAFile: p.caFile,
		},
		LogLevel: "ERROR",
	}
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// startWriter serves a writer on two loopback ports and returns their addresses.
func startWriter(t *testing.T, cfg *config.ScoreWriterConfig) (publishAddr, internalAddr string) {
	t.Helper()
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	publishLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	internalLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Serve(publishLn, internalLn) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return publishLn.Addr().String(), internalLn.Addr().String()
}

func httpsClient(roots *x509.CertPool, maxVersion uint16) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS10, MaxVersion: maxVersion},
		},
	}
}

func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestRun_MissingTLSFileFailsStartup(t *testing.T) {
	p := newTestPKI(t)
	for _, missing := range []string{"cert", "key"} {
		t.Run(missing, func(t *testing.T) {
			cfg := testConfig(t, p)
			if missing == "cert" {
				cfg.TLS.CertFile = filepath.Join(t.TempDir(), "absent.crt")
			} else {
				cfg.TLS.KeyFile = filepath.Join(t.TempDir(), "absent.key")
			}
			err := Run(cfg)
			if err == nil || !strings.Contains(err.Error(), "load the TLS key pair") {
				t.Fatalf("Run err = %v, want a TLS key pair error", err)
			}
		})
	}
}

func TestNew_MissingDatabaseFilesFailStartup(t *testing.T) {
	p := newTestPKI(t)
	tests := []struct {
		name    string
		mutate  func(*config.ScoreWriterConfig)
		wantErr string
	}{
		{"password file", func(c *config.ScoreWriterConfig) { c.Database.PasswordFile = "/nonexistent/password" }, "read the score database password"},
		{"CA file", func(c *config.ScoreWriterConfig) { c.Database.CAFile = "/nonexistent/ca.crt" }, "score database config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, p)
			tt.mutate(cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestServe_ProbesOnInternalPortOnly(t *testing.T) {
	p := newTestPKI(t)
	publishAddr, internalAddr := startWriter(t, testConfig(t, p))
	client := httpsClient(p.roots, tls.VersionTLS13)

	if code, body := get(t, client, "https://"+internalAddr+"/healthz"); code != http.StatusOK || body != `{"status":"healthy"}` {
		t.Errorf("internal /healthz = %d %s, want 200 healthy", code, body)
	}
	// No database listens, so the writer starts but isn't ready.
	if code, body := get(t, client, "https://"+internalAddr+"/readyz"); code != http.StatusServiceUnavailable || body != `{"status":"unavailable"}` {
		t.Errorf("internal /readyz = %d %s, want 503 unavailable", code, body)
	}
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		if code, _ := get(t, client, "https://"+publishAddr+path); code != http.StatusNotFound {
			t.Errorf("publish %s = %d, want 404", path, code)
		}
	}
	if code, _ := get(t, client, "https://"+publishAddr+"/api/v1/publisher/monitors/m/runs/r/scores"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the publish route = %d, want 405", code)
	}
}

func TestServe_AcceptsTLS12(t *testing.T) {
	p := newTestPKI(t)
	_, internalAddr := startWriter(t, testConfig(t, p))

	if code, _ := get(t, httpsClient(p.roots, tls.VersionTLS12), "https://"+internalAddr+"/healthz"); code != http.StatusOK {
		t.Errorf("TLS 1.2 /healthz = %d, want 200", code)
	}
}

func TestServe_RefusesTLS11(t *testing.T) {
	p := newTestPKI(t)
	publishAddr, internalAddr := startWriter(t, testConfig(t, p))

	for name, addr := range map[string]string{"publish": publishAddr, "internal": internalAddr} {
		t.Run(name, func(t *testing.T) {
			conn, err := tls.Dial("tcp", addr, &tls.Config{
				RootCAs: p.roots, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11,
			})
			if err == nil {
				_ = conn.Close()
				t.Fatal("TLS 1.1 handshake succeeded")
			}
		})
	}
}

func TestServe_RefusesPlainHTTP(t *testing.T) {
	p := newTestPKI(t)
	publishAddr, internalAddr := startWriter(t, testConfig(t, p))
	client := &http.Client{Timeout: 5 * time.Second}

	for name, addr := range map[string]string{"publish": publishAddr, "internal": internalAddr} {
		t.Run(name, func(t *testing.T) {
			code, body := get(t, client, "http://"+addr+"/healthz")
			if code != http.StatusBadRequest || !strings.Contains(body, "HTTP request to an HTTPS server") {
				t.Fatalf("plain HTTP = %d %q, want 400 from the TLS listener", code, body)
			}
		})
	}
}
