package waved

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/serverconn"
	"google.golang.org/grpc"
)

// operatorClients holds the daemon-owned outbound clients used to talk to the
// Ark operator directly and through the mailbox edge.
type operatorClients struct {
	conn    *grpc.ClientConn
	ark     arkrpc.ArkServiceClient
	mailbox mailboxpb.MailboxServiceClient
	cleanup func() error
}

// connectOperatorClients builds the outbound clients for the configured
// operator transport.
//
// Both mailbox edges are wrapped so every Send, Pull and AckUpTo carries the
// x-mailbox-auth-sig metadata header. The operator authorizes a mailbox RPC
// either from the TLS client certificate bound to the caller's mailbox ID or
// from that header, and an operator that terminates TLS at a proxy never sees
// a client certificate. Signing unconditionally means the daemon works against
// either posture without the operator's TLS choice leaking into client config.
func (s *Server) connectOperatorClients() (*operatorClients, error) {
	sign := s.mailboxAuthSigner()

	switch s.cfg.Server.Transport {
	case "", RPCTransportGRPC:
		conn, err := s.dialServer()
		if err != nil {
			return nil, err
		}

		return &operatorClients{
			conn: conn,
			ark:  arkrpc.NewArkServiceClient(conn),
			mailbox: serverconn.NewAuthenticatedMailboxClient(
				mailboxpb.NewMailboxServiceClient(conn), sign,
			),
			cleanup: conn.Close,
		}, nil

	case RPCTransportREST:
		opts, err := s.operatorRESTOptions()
		if err != nil {
			return nil, err
		}

		transport := restclient.New(
			operatorRESTBaseURL(
				s.cfg.Server, s.cfg.ArkServerAddress(),
			),
			opts...,
		)

		return &operatorClients{
			ark: restclient.NewArkServiceClientFromClient(
				transport,
			),
			mailbox: serverconn.NewAuthenticatedMailboxClient(
				restclient.NewMailboxServiceClientFromClient(
					transport,
				),
				sign,
			),
			cleanup: func() error { return nil },
		}, nil

	default:
		return nil, fmt.Errorf("unknown server transport %q",
			s.cfg.Server.Transport)
	}
}

// mailboxAuthSigner returns the signer that stamps x-mailbox-auth-sig on
// outbound mailbox RPCs. It reads the identity key lazily, per RPC, because
// the mailbox edge is built before the wallet has derived one.
func (s *Server) mailboxAuthSigner() serverconn.MailboxAuthSigner {
	return func(ctx context.Context, recipientMailboxID string) (string,
		error) {

		sig, err := s.mailboxAuthSig(ctx, recipientMailboxID)
		if err != nil {
			return "", err
		}

		return hex.EncodeToString(sig.Serialize()), nil
	}
}

// mailboxAuthSig returns the mailbox auth signature for recipientMailboxID,
// signing once per recipient and serving every later call from memory.
//
// Memoizing is what makes signing per RPC affordable: the digest is
// TaggedHash("mailbox-auth", identityPubKey || recipientMailboxID), so it does
// not vary with the request, while signMailboxAuth costs a round-trip to
// whichever wallet backend holds the key.
//
// The map holds two entries, not one. Send addresses the compound
// operator:client mailbox, while Pull and AckUpTo address this client's own
// plain mailbox ID, so the two arms sign different recipients and cache
// separately. Keying on the recipient is what keeps them from colliding.
func (s *Server) mailboxAuthSig(ctx context.Context,
	recipientMailboxID string) (*schnorr.Signature, error) {

	if s.clientKeyDesc.PubKey == nil {
		return nil, fmt.Errorf("identity key not yet derived; wallet " +
			"not ready")
	}

	s.mailboxAuthSigsMu.Lock()
	defer s.mailboxAuthSigsMu.Unlock()

	if sig, ok := s.mailboxAuthSigs[recipientMailboxID]; ok {
		return sig, nil
	}

	sig, err := s.signMailboxAuth(ctx, recipientMailboxID)
	if err != nil {
		return nil, err
	}

	if s.mailboxAuthSigs == nil {
		s.mailboxAuthSigs = make(map[string]*schnorr.Signature, 1)
	}
	s.mailboxAuthSigs[recipientMailboxID] = sig

	return sig, nil
}

// serverClientTLSCerts returns the optional client certificate used by the
// operator to bind mailbox access to the daemon identity key.
func (s *Server) serverClientTLSCerts() ([]tls.Certificate, error) {
	if s.cfg.Server.Insecure || s.clientKeyDesc.PubKey == nil {
		return nil, nil
	}

	clientCert, err := serverconn.GenerateClientTLSCert(
		s.clientKeyDesc.PubKey,
	)
	if err != nil {
		return nil, fmt.Errorf("generate client TLS cert: %w", err)
	}

	// Cache the leaf SubjectPublicKeyInfo bytes so the mailbox
	// transport can sign over them and the server can verify the
	// secp256k1 identity is bound to the TLS leaf it observes
	// (issue #448).
	if clientCert.Leaf != nil {
		s.tlsLeafSPKI = clientCert.Leaf.RawSubjectPublicKeyInfo
	}

	return []tls.Certificate{clientCert}, nil
}

// operatorRESTOptions maps the operator TLS config into the shared REST
// transport. In insecure mode a bare host is normalized to http by
// operatorRESTBaseURL, so no custom client is needed.
func (s *Server) operatorRESTOptions() ([]restclient.Option, error) {
	clientCerts, err := s.serverClientTLSCerts()
	if err != nil {
		return nil, err
	}

	tlsCfg := &tls.Config{
		Certificates: clientCerts,
		MinVersion:   tls.VersionTLS12,
	}
	if s.cfg.Server.TLSCertPath != "" {
		certBytes, err := os.ReadFile(s.cfg.Server.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read server TLS "+
				"cert: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certBytes) {
			return nil, fmt.Errorf("unable to parse server TLS "+
				"cert at %s", s.cfg.Server.TLSCertPath)
		}
		tlsCfg.RootCAs = pool
	}

	httpTransport := cloneDefaultHTTPTransport()
	httpTransport.TLSClientConfig = tlsCfg

	opts := []restclient.Option{
		restclient.WithHTTPClient(&http.Client{
			Transport: httpTransport,
		}),
	}
	if s.cfg.Server.MacaroonPath != "" {
		macHex, err := rpcauth.HexFromFile(s.cfg.Server.MacaroonPath)
		if err != nil {
			return nil, err
		}

		opts = append(
			opts, restclient.WithHeader(
				rpcauth.MacaroonMetadataKey, macHex,
			),
		)
	}

	return opts, nil
}

// cloneDefaultHTTPTransport returns a mutable copy of the default HTTP
// transport without relying on a forced package-global type assertion.
func cloneDefaultHTTPTransport() *http.Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		}
	}

	return transport.Clone()
}

// operatorRESTBaseURL returns the base URL used for grpc-gateway calls.
func operatorRESTBaseURL(cfg *ServerConfig, addr string) string {
	if strings.HasPrefix(addr, "http://") ||
		strings.HasPrefix(addr, "https://") {
		return addr
	}

	if cfg.Insecure {
		return "http://" + addr
	}

	return "https://" + addr
}

// operatorArkClient returns the configured ArkService client, preserving the
// old direct gRPC test hook when tests set serverConn by hand.
func (s *Server) operatorArkClient() arkrpc.ArkServiceClient {
	if s.arkClient != nil {
		return s.arkClient
	}
	if s.serverConn != nil {
		return arkrpc.NewArkServiceClient(s.serverConn)
	}

	return nil
}
