package devrpc

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type testDaemonServer struct {
	waverpc.UnimplementedDaemonServiceServer

	listReq    *waverpc.ListVTXOsRequest
	sendReq    *waverpc.SendVTXORequest
	oorReq     *waverpc.SendOORRequest
	refreshReq *waverpc.RefreshVTXOsRequest
	authReq    *waverpc.ReceiveAuthKeyRequest
}

func (s *testDaemonServer) GetInfo(context.Context, *waverpc.GetInfoRequest) (
	*waverpc.GetInfoResponse, error) {

	return &waverpc.GetInfoResponse{
		Version: "test-version",
	}, nil
}

func (s *testDaemonServer) ListVTXOs(_ context.Context,
	req *waverpc.ListVTXOsRequest) (*waverpc.ListVTXOsResponse, error) {

	s.listReq = req

	return &waverpc.ListVTXOsResponse{}, nil
}

func (s *testDaemonServer) SendVTXO(_ context.Context,
	req *waverpc.SendVTXORequest) (*waverpc.SendVTXOResponse, error) {

	s.sendReq = req

	return &waverpc.SendVTXOResponse{
		Status: "accepted",
	}, nil
}

func (s *testDaemonServer) SendOOR(_ context.Context,
	req *waverpc.SendOORRequest) (*waverpc.SendOORResponse, error) {

	s.oorReq = req

	return &waverpc.SendOORResponse{
		Status: "accepted",
	}, nil
}

func (s *testDaemonServer) RefreshVTXOs(_ context.Context,
	req *waverpc.RefreshVTXOsRequest) (*waverpc.RefreshVTXOsResponse,
	error) {

	s.refreshReq = req

	return &waverpc.RefreshVTXOsResponse{
		Status: "accepted",
	}, nil
}

func (s *testDaemonServer) ReceiveAuthKey(_ context.Context,
	req *waverpc.ReceiveAuthKeyRequest) (*waverpc.ReceiveAuthKeyResponse,
	error) {

	s.authReq = req

	return &waverpc.ReceiveAuthKeyResponse{}, nil
}

func TestDevCmdInvokesUnaryRPC(t *testing.T) {
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, &testDaemonServer{}, &out)
	defer cleanup()

	cmd.SetArgs([]string{"daemon", "get-info"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev getinfo: %v", err)
	}

	if !strings.Contains(out.String(), `"test-version"`) {
		t.Fatalf("expected getinfo response, got %s", out.String())
	}
}

func TestDevCmdParsesScalarAndEnumFlags(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "list-vtxos",
		"--status_filter", "live",
		"--min_amount_sat", "10",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev list-vtxos: %v", err)
	}

	if server.listReq == nil {
		t.Fatalf("expected ListVTXOs request")
	}
	if server.listReq.StatusFilter !=
		waverpc.VTXOStatus_VTXO_STATUS_LIVE {

		t.Fatalf("status filter = %v", server.listReq.StatusFilter)
	}
	if server.listReq.MinAmountSat != 10 {
		t.Fatalf("min amount = %v", server.listReq.MinAmountSat)
	}
}

func TestDevCmdHelpShowsEnumValues(t *testing.T) {
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, &testDaemonServer{}, &out)
	defer cleanup()

	var help bytes.Buffer
	cmd.SetOut(&help)
	cmd.SetErr(&help)
	cmd.SetArgs([]string{"btcwallet", "next-address", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev btcwallet next-address help: %v", err)
	}

	got := help.String()
	for _, want := range []string{
		"--kind string",
		"BIP0044_EXTERNAL",
		"BIP0044_INTERNAL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected help to contain %q, got:\n%s", want,
				got)
		}
	}
}

func TestDevCmdParsesComplexJSONAndBoolFlags(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "send-vtxo",
		"--recipients-json", `[{"address":"bcrt1dest","amount_sat":5}]`,
		"--dry_run",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev send-vtxo: %v", err)
	}

	if server.sendReq == nil {
		t.Fatalf("expected SendVTXO request")
	}
	if !server.sendReq.DryRun {
		t.Fatalf("expected dry_run to be set")
	}
	if len(server.sendReq.Recipients) != 1 {
		t.Fatalf("recipient count = %v", len(server.sendReq.Recipients))
	}
	if server.sendReq.Recipients[0].AmountSat != 5 {
		t.Fatalf("amount = %v", server.sendReq.Recipients[0].AmountSat)
	}

	dest, ok := server.sendReq.Recipients[0].
		Destination.(*waverpc.Output_Address)
	if !ok {
		t.Fatalf("destination = %T", server.sendReq.Recipients[0].
			Destination)
	}
	if dest.Address != "bcrt1dest" {
		t.Fatalf("address = %v", dest.Address)
	}
}

func TestDevCmdRejectsBadFieldJSON(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "send-vtxo",
		"--recipients-json", `[{"bogus":1}]`,
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected JSON field error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestDevCmdRejectsEmptyFieldJSON(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "send-vtxo",
		"--recipients-json", "",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected empty JSON field error")
	}
	if !strings.Contains(err.Error(), "missing value") {
		t.Fatalf("expected missing value error, got %v", err)
	}
}

func TestDevCmdParsesHexBytesWithSpaces(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "receive-auth-key",
		"--payment_hash", "00 11",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev receive-auth-key: %v", err)
	}

	if server.authReq == nil {
		t.Fatalf("expected ReceiveAuthKey request")
	}

	got := server.authReq.PaymentHash
	if len(got) != 2 || got[0] != 0x00 || got[1] != 0x11 {
		t.Fatalf("payment hash = %x", got)
	}
}

func TestDevCmdParsesSendOORRecipients(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "send-oor",
		"--recipients-json", `[{"address":"bcrt1dest","amount_sat":7}]`,
		"--dry_run",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev send-oor: %v", err)
	}

	if server.oorReq == nil {
		t.Fatalf("expected SendOOR request")
	}
	if !server.oorReq.DryRun {
		t.Fatalf("expected dry_run to be set")
	}
	if len(server.oorReq.Recipients) != 1 {
		t.Fatalf("recipient count = %v", len(server.oorReq.Recipients))
	}
	if server.oorReq.Recipients[0].AmountSat != 7 {
		t.Fatalf("amount = %v", server.oorReq.Recipients[0].AmountSat)
	}

	dest, ok := server.oorReq.Recipients[0].
		Destination.(*waverpc.Output_Address)
	if !ok {
		t.Fatalf("destination = %T", server.oorReq.Recipients[0].
			Destination)
	}
	if dest.Address != "bcrt1dest" {
		t.Fatalf("address = %v", dest.Address)
	}
}

func TestDevCmdRejectsNestedOneofConflicts(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "prepare-oor",
		"--recipient.address", "bcrt1dest",
		"--recipient.pubkey", "00",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected oneof conflict")
	}
	if !strings.Contains(err.Error(), "set the same oneof") {
		t.Fatalf("expected oneof conflict, got %v", err)
	}
}

func TestDevCmdFlattensNestedRepeatedScalars(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.SetArgs([]string{
		"daemon", "refresh-vtxos",
		"--outpoints.outpoints", "txid:0",
		"--outpoints.outpoints", "txid:1",
		"--dry_run",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev refresh-vtxos: %v", err)
	}

	if server.refreshReq == nil {
		t.Fatalf("expected RefreshVTXOs request")
	}

	outpoints, ok := server.refreshReq.
		Selection.(*waverpc.RefreshVTXOsRequest_Outpoints)
	if !ok {
		t.Fatalf("selection = %T", server.refreshReq.Selection)
	}

	got := outpoints.Outpoints.Outpoints
	if len(got) != 2 || got[0] != "txid:0" || got[1] != "txid:1" {
		t.Fatalf("outpoints = %v", got)
	}
}

func TestDevCmdAcceptsRawJSONRequest(t *testing.T) {
	server := &testDaemonServer{}
	var out bytes.Buffer
	cmd, cleanup := newTestDevCmd(t, server, &out)
	defer cleanup()

	cmd.PersistentFlags().String("json", "", "raw JSON request payload")
	cmd.SetArgs([]string{
		"daemon", "list-vtxos",
		"--json", `{"status_filter":"VTXO_STATUS_SPENT"}`,
		"--min_amount_sat", "10",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute dev list-vtxos with json: %v", err)
	}

	if server.listReq == nil {
		t.Fatalf("expected ListVTXOs request")
	}
	if server.listReq.StatusFilter !=
		waverpc.VTXOStatus_VTXO_STATUS_SPENT {

		t.Fatalf("status filter = %v", server.listReq.StatusFilter)
	}
	if server.listReq.MinAmountSat != 0 {
		t.Fatalf("expected --json to ignore flags, min amount = %v",
			server.listReq.MinAmountSat)
	}
}

func newTestDevCmd(t *testing.T, server waverpc.DaemonServiceServer,
	out *bytes.Buffer) (*cobra.Command, func()) {

	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	waverpc.RegisterDaemonServiceServer(grpcServer, server)

	errChan := make(chan error, 1)
	go func() {
		errChan <- grpcServer.Serve(listener)
	}()

	cfg := Config{
		GetConn: func(*cobra.Command) (*grpc.ClientConn, error) {
			return grpc.NewClient(
				"passthrough:///bufnet",
				grpc.WithContextDialer(func(context.Context,
					string) (net.Conn, error) {

					return listener.Dial()
				}),
				grpc.WithTransportCredentials(
					insecure.NewCredentials(),
				),
			)
		},
		PrintJSON: func(msg proto.Message) error {
			data, err := protojson.MarshalOptions{
				Indent:          "  ",
				UseProtoNames:   true,
				EmitUnpopulated: true,
			}.Marshal(
				msg,
			)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(out, string(data))

			return err
		},
	}

	cleanup := func() {
		grpcServer.Stop()
		_ = listener.Close()
		<-errChan
	}

	cmd := NewDevCmd(cfg)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	return cmd, cleanup
}
