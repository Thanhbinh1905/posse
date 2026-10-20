//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testReleaseAsset struct {
	tag         string
	archive     []byte
	archiveName string
	checksum    string
}

func startUpdateReleaseProxy(t *testing.T, asset testReleaseAsset) (string, string) {
	t.Helper()
	certificate, certPath := newUpdateProxyCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}
		tlsConnection := tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		defer tlsConnection.Close()
		if err := tlsConnection.Handshake(); err != nil {
			return
		}
		tunnelRequest, err := http.ReadRequest(bufio.NewReader(tlsConnection))
		if err != nil {
			return
		}
		defer tunnelRequest.Body.Close()
		status, contentType, body := updateProxyResponse(tunnelRequest.URL.Path, asset)
		response := &http.Response{
			StatusCode:    status,
			Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Close:         true,
		}
		response.Header.Set("Content-Type", contentType)
		_ = response.Write(tlsConnection)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String(), certPath
}

func updateProxyResponse(path string, asset testReleaseAsset) (int, string, []byte) {
	if strings.HasSuffix(path, "/releases/tags/"+asset.tag) {
		metadata := fmt.Sprintf(`{"tag_name":%q,"body":"E2E update fixture","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
			asset.tag,
			asset.archiveName,
			"https://api.github.com/assets/"+asset.tag+"/archive",
			"https://api.github.com/assets/"+asset.tag+"/checksums",
		)
		return http.StatusOK, "application/json", []byte(metadata)
	}
	if path == "/assets/"+asset.tag+"/archive" {
		return http.StatusOK, "application/octet-stream", asset.archive
	}
	if path == "/assets/"+asset.tag+"/checksums" {
		return http.StatusOK, "text/plain", []byte(asset.checksum + "  " + asset.archiveName + "\n")
	}
	return http.StatusNotFound, "text/plain", []byte("not found: " + path)
}

func newUpdateProxyCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "api.github.com"},
		DNSNames:              []string{"api.github.com"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "update-proxy-ca.pem")
	if err := os.WriteFile(certPath, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certificate, certPath
}
