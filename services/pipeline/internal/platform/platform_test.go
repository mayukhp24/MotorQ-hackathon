package platform

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCert creates a self-signed certificate and key as PEM files.
func writeCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gateway"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return certFile, keyFile
}

func TestClientTLSDisabledByDefault(t *testing.T) {
	cfg, err := ClientTLS("TESTX")
	if cfg != nil || err != nil {
		t.Fatalf("want nil, nil; got %v, %v", cfg, err)
	}
}

func TestClientTLSMutual(t *testing.T) {
	cert, key := writeCert(t, t.TempDir())
	t.Setenv("MQTT_TLS", "true")
	t.Setenv("MQTT_TLS_CA_FILE", cert)
	t.Setenv("MQTT_TLS_CERT_FILE", cert)
	t.Setenv("MQTT_TLS_KEY_FILE", key)
	cfg, err := ClientTLS("MQTT")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || cfg.RootCAs == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("incomplete TLS config: %+v", cfg)
	}
}

func TestClientTLSErrors(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(junk, []byte("not a cert"), 0o600)
	t.Setenv("KAFKA_TLS", "true")
	t.Setenv("KAFKA_TLS_CA_FILE", junk)
	if _, err := ClientTLS("KAFKA"); err == nil {
		t.Fatal("expected error for CA file without certificates")
	}
	t.Setenv("KAFKA_TLS_CA_FILE", filepath.Join(dir, "missing.pem"))
	if _, err := ClientTLS("KAFKA"); err == nil {
		t.Fatal("expected error for missing CA file")
	}
	t.Setenv("KAFKA_TLS_CA_FILE", "")
	t.Setenv("KAFKA_TLS_CERT_FILE", junk)
	t.Setenv("KAFKA_TLS_KEY_FILE", junk)
	if _, err := ClientTLS("KAFKA"); err == nil {
		t.Fatal("expected error for bad client key pair")
	}
}

func TestKafkaSecurity(t *testing.T) {
	t.Setenv("KAFKA_SASL_MECHANISM", "scram-sha-512")
	t.Setenv("KAFKA_SASL_USERNAME", "u")
	t.Setenv("KAFKA_SASL_PASSWORD", "p")
	opts, err := kafkaSecurity()
	if err != nil || len(opts) != 1 {
		t.Fatalf("scram: %v %d", err, len(opts))
	}
	t.Setenv("KAFKA_SASL_MECHANISM", "PLAIN")
	if _, err := kafkaSecurity(); err == nil {
		t.Fatal("expected unsupported mechanism error")
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("X_INT", "7")
	t.Setenv("X_BAD", "abc")
	t.Setenv("X_DUR", "3s")
	t.Setenv("X_F", "0.5")
	if EnvInt("X_INT", 1) != 7 || EnvInt("X_BAD", 1) != 1 || EnvDuration("X_DUR", 0) != 3*time.Second ||
		EnvFloat("X_F", 0) != 0.5 || Env("X_NONE", "d") != "d" {
		t.Fatal("env helpers")
	}
}
