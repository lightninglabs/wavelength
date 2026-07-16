package restclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	jsonMarshal = protojson.MarshalOptions{
		UseProtoNames: true,
	}
	jsonUnmarshal = protojson.UnmarshalOptions{
		DiscardUnknown: true,
	}
)

// Option configures a REST RPC client transport.
type Option func(*Client)

// Client is the shared HTTP/protoJSON transport used by generated-service
// shaped REST clients.
type Client struct {
	baseURL    string
	httpClient *http.Client
	headers    http.Header
}

// New creates a REST RPC client transport for one grpc-gateway base address.
func New(addr string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(defaultBaseURL(addr), "/"),
		httpClient: http.DefaultClient,
		headers:    make(http.Header),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}

	return c
}

// defaultBaseURL infers a conservative scheme for schemeless gateway
// addresses. Loopback development endpoints use HTTP; every other host
// defaults to HTTPS so hosted browser wallets do not silently send auth over
// plaintext.
func defaultBaseURL(addr string) string {
	if strings.HasPrefix(addr, "http://") ||
		strings.HasPrefix(addr, "https://") {
		return addr
	}

	if isLoopbackAddress(addr) {
		return "http://" + addr
	}

	return "https://" + addr
}

// isLoopbackAddress reports whether addr names a loopback HTTP endpoint.
func isLoopbackAddress(addr string) bool {
	host := addr
	if splitHost, _, err := net.SplitHostPort(addr); err == nil {
		host = splitHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// WithHTTPClient makes the REST transport use the supplied HTTP client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// WithHeader adds a static HTTP header to every request.
func WithHeader(key string, values ...string) Option {
	return func(c *Client) {
		for _, value := range values {
			c.headers.Add(key, value)
		}
	}
}

// Post sends one unary grpc-gateway request and unmarshals the response.
func (c *Client) Post(ctx context.Context, path string, in proto.Message,
	out proto.Message) error {

	body, err := jsonMarshal.Marshal(in)
	if err != nil {
		return err
	}

	req, err := c.newRequest(
		ctx, http.MethodPost, path, bytes.NewReader(body),
	)
	if err != nil {
		return err
	}

	return c.doUnary(req, out)
}

// Get sends one unary grpc-gateway GET request and unmarshals the response.
// The path must already carry any query string the endpoint binds its request
// fields from, since a GET request carries no body. This is required for
// grpc-gateway services (such as lnd's ChainKit and Lightning services) that
// expose read RPCs over HTTP GET rather than POST.
func (c *Client) Get(ctx context.Context, path string,
	out proto.Message) error {

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	return c.doUnary(req, out)
}

// doUnary executes a prepared unary HTTP request and decodes the JSON response
// into out, mapping any gateway HTTP error into a gRPC status error. It is the
// shared response-handling core of Post and Get.
func (c *Client) doUnary(req *http.Request, out proto.Message) error {
	//nolint:gosec // The caller explicitly configures the gateway URL.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK ||
		resp.StatusCode >= http.StatusMultipleChoices {
		return GatewayStatusError(resp.StatusCode, respBody)
	}

	return jsonUnmarshal.Unmarshal(respBody, out)
}

// Stream opens one grpc-gateway server-streaming request.
func (c *Client) Stream(ctx context.Context, path string, in proto.Message) (
	*http.Response, error) {

	body, err := jsonMarshal.Marshal(in)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(
		ctx, http.MethodPost, path, bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // The caller explicitly configures the gateway URL.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusOK &&
		resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return nil, GatewayStatusError(resp.StatusCode, respBody)
}

func (c *Client) newRequest(ctx context.Context, method string, path string,
	body io.Reader) (*http.Request, error) {

	req, err := http.NewRequestWithContext(
		ctx, method, c.baseURL+path, body,
	)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, values := range c.headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for key, values := range md {
			if strings.HasSuffix(strings.ToLower(key), "-bin") {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}

	return req, nil
}

// GatewayError is the grpc-gateway JSON error envelope.
type GatewayError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

// GatewayStatusError converts one grpc-gateway HTTP error response into a
// gRPC status error.
func GatewayStatusError(httpStatus int, body []byte) error {
	var gwErr GatewayError
	if err := json.Unmarshal(body, &gwErr); err != nil {
		return status.Errorf(codeFromHTTPStatus(httpStatus), "http "+
			"%d: %s", httpStatus, string(body))
	}

	code := codeFromHTTPStatus(httpStatus)
	if len(gwErr.Code) > 0 {
		code = codeFromJSON(gwErr.Code, code)
	}
	if gwErr.Message == "" {
		gwErr.Message = strings.TrimSpace(string(body))
	}

	return status.Error(code, gwErr.Message)
}

func codeFromJSON(raw json.RawMessage, fallback codes.Code) codes.Code {
	var codeInt int
	if err := json.Unmarshal(raw, &codeInt); err == nil {
		return codes.Code(codeInt)
	}

	var codeString string
	if err := json.Unmarshal(raw, &codeString); err == nil {
		if parsed, err := strconv.Atoi(codeString); err == nil {
			return codes.Code(parsed)
		}
	}

	return fallback
}

func codeFromHTTPStatus(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest:
		return codes.InvalidArgument

	case http.StatusUnauthorized:
		return codes.Unauthenticated

	case http.StatusForbidden:
		return codes.PermissionDenied

	case http.StatusNotFound:
		return codes.NotFound

	case http.StatusConflict:
		return codes.Aborted

	case http.StatusTooManyRequests:
		return codes.ResourceExhausted

	case 499:
		return codes.Canceled

	case http.StatusNotImplemented:
		return codes.Unimplemented

	case http.StatusServiceUnavailable:
		return codes.Unavailable

	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded

	default:
		return codes.Unknown
	}
}

func unsupportedSendMsg(name string) error {
	return fmt.Errorf("rest %s stream does not support SendMsg", name)
}
