package serverconn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
)

// GenerateClientTLSCert creates a self-signed P-256 TLS certificate
// with the secp256k1 identity pubkey hex as the Subject CommonName.
// The cert is ephemeral (generated fresh at each startup) and serves
// two purposes:
//
//  1. It authenticates the TLS connection so the server can enforce
//     per-RPC access control (Pull/AckUpTo restricted to the
//     client's own mailbox).
//  2. The CN carries the client's secp256k1 identity so the server
//     can extract it from the TLS peer state.
//
// The actual proof of secp256k1 key ownership is provided by the
// Schnorr auth signature in envelope headers, not by this cert.
func GenerateClientTLSCert(identityPubKey *btcec.PublicKey) (tls.Certificate,
	error) {

	if identityPubKey == nil {
		return tls.Certificate{}, fmt.Errorf("identity public key " +
			"must not be nil")
	}

	// Generate a fresh P-256 key for TLS transport. This is
	// separate from the secp256k1 identity key because Go's
	// x509 package does not support secp256k1 certificates.
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate P-256 "+
			"TLS key: %w", err)
	}

	serialNumber, err := rand.Int(
		rand.Reader,
		new(
			big.Int).Lsh(big.NewInt(1), 128),
	)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial "+
			"number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: PubKeyMailboxID(identityPubKey),
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader, template, template, &tlsKey.PublicKey, tlsKey,
	)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create client TLS "+
			"certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse client TLS "+
			"certificate: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{
			certDER,
		},
		PrivateKey: tlsKey,
		Leaf:       leaf,
	}, nil
}
