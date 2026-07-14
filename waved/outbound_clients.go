package waved

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"

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
func (s *Server) connectOperatorClients() (*operatorClients, error) {
	switch s.cfg.Server.Transport {
	case "", RPCTransportGRPC:
		conn, err := s.dialServer()
		if err != nil {
			return nil, err
		}

		return &operatorClients{
			conn:    conn,
			ark:     arkrpc.NewArkServiceClient(conn),
			mailbox: mailboxpb.NewMailboxServiceClient(conn),
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
			mailbox: restclient.NewMailboxServiceClientFromClient(
				transport,
			),
			cleanup: func() error { return nil },
		}, nil

	default:
		return nil, fmt.Errorf("unknown server transport %q",
			s.cfg.Server.Transport)
	}
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
