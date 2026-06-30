package darepod

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const darepodMacaroonEntity = "darepod"

var (
	darepodReadOp = bakery.Op{
		Entity: darepodMacaroonEntity,
		Action: "read",
	}
	darepodWriteOp = bakery.Op{
		Entity: darepodMacaroonEntity,
		Action: "write",
	}
)

var darepodRPCPermissions = newDarepodRPCPermissions()

// newDarepodRPCPermissions returns the local daemon's explicit macaroon policy.
func newDarepodRPCPermissions() map[string][]bakery.Op {
	read := []bakery.Op{darepodReadOp}
	write := []bakery.Op{darepodWriteOp}

	permissions := make(map[string][]bakery.Op)
	addService := func(service string, methods map[string][]bakery.Op) {
		for method, ops := range methods {
			fullMethod := "/" + service + "/" + method
			permissions[fullMethod] = ops
		}
	}

	addService(
		daemonrpc.DaemonService_ServiceDesc.ServiceName,
		map[string][]bakery.Op{
			"GetInfo":                       read,
			"GenSeed":                       write,
			"InitWallet":                    write,
			"UnlockWallet":                  write,
			"GetBalance":                    read,
			"ListVTXOs":                     read,
			"NewAddress":                    write,
			"NewReceiveScript":              write,
			"ReceiveAuthKey":                write,
			"SignReceiveAuthMessage":        write,
			"SignReceiveAuthMessageCompact": write,
			"ReceiveAuthECDH":               write,
			"GetIndexedVTXOByPkScript":      read,
			"GetVTXOExpiryInfo":             read,
			"GetIndexedOORSessionByTxid":    read,
			"SendVTXO":                      write,
			"SendOOR":                       write,
			"PrepareOOR":                    write,
			"SignOORCustomInput":            write,
			"SignVTXOForfeit":               write,
			"RefreshVTXOs":                  write,
			"RefreshCustomVTXOs":            write,
			"ListPendingForfeitParticipantSignatureRequests": read,
			"SubmitForfeitParticipantSignatures":             write,
			"LeaveVTXOs":                                     write,
			"SendOnChain":                                    write,
			"Board":                                          write,
			"JoinNextRound":                                  write,
			"SweepBoardingUTXOs":                             write,
			"ListBoardingSweeps":                             read,
			"ListRounds":                                     read,
			"GetRound":                                       read,
			"WatchRounds":                                    read,
			"ListOORSessions":                                read,
			"GetOORSession":                                  read,
			"EstimateFee":                                    read,
			"GetFeeHistory":                                  read,
			"ListTransactions":                               read,
			"Unroll":                                         write,
			"GetUnrollStatus":                                read,
			"ArmVHTLCRecovery":                               write,
			"EscalateVHTLCRecovery":                          write,
			"CancelVHTLCRecovery":                            write,
			"GetVHTLCRecoveryStatus":                         read,
			"ListVHTLCRecoveries":                            read,
		},
	)

	addService(
		swapclientrpc.SwapClientService_ServiceDesc.ServiceName,
		map[string][]bakery.Op{
			"QuotePay":       read,
			"StartPay":       write,
			"StartReceive":   write,
			"ResumeSwap":     write,
			"ListSwaps":      read,
			"GetSwap":        read,
			"SubscribeSwaps": read,
		},
	)

	addService(
		walletdkrpc.WalletService_ServiceDesc.ServiceName,
		map[string][]bakery.Op{
			"Create":          write,
			"Unlock":          write,
			"PrepareSend":     write,
			"Send":            write,
			"Recv":            write,
			"List":            read,
			"Deposit":         write,
			"Balance":         read,
			"Status":          read,
			"GetExitPlan":     read,
			"SweepWallet":     write,
			"Exit":            write,
			"ExitStatus":      read,
			"SubscribeWallet": read,
		},
	)
	addService(
		walletdkrpc.WalletInspectionService_ServiceDesc.ServiceName,
		map[string][]bakery.Op{
			"InspectActivity": read,
		},
	)

	addService("walletrpc.VersionService", map[string][]bakery.Op{
		"Version": read,
	})
	addService("walletrpc.WalletService", map[string][]bakery.Op{
		"Ping":                     read,
		"Network":                  read,
		"AccountNumber":            read,
		"Accounts":                 read,
		"Balance":                  read,
		"GetTransactions":          read,
		"ChangePassphrase":         write,
		"RenameAccount":            write,
		"NextAccount":              write,
		"NextAddress":              write,
		"ImportPrivateKey":         write,
		"FundTransaction":          write,
		"SignTransaction":          write,
		"PublishTransaction":       write,
		"TransactionNotifications": read,
		"SpentnessNotifications":   read,
		"AccountNotifications":     read,
	})

	return permissions
}

// registeredRPCPermissions maps every registered gRPC method to the macaroon
// permission it requires.
func registeredRPCPermissions(grpcServer *grpc.Server) (map[string][]bakery.Op,
	error) {

	info := grpcServer.GetServiceInfo()
	permissions := make(map[string][]bakery.Op)

	for serviceName, serviceInfo := range info {
		for _, method := range serviceInfo.Methods {
			fullMethod := "/" + serviceName + "/" + method.Name
			ops, ok := darepodRPCPermissions[fullMethod]
			if !ok {
				return nil, fmt.Errorf("no macaroon "+
					"permission registered for %s",
					fullMethod)
			}

			permissions[fullMethod] = append(
				[]bakery.Op(nil), ops...,
			)
		}
	}

	return permissions, nil
}
