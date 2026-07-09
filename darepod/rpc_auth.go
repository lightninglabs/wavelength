package darepod

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// darepodMacaroonLocation is the "Location" field stamped into every macaroon
// the daemon bakes. It identifies the issuing daemon, not a permission scope.
const darepodMacaroonLocation = "darepod"

// Macaroon entities slice the daemon's RPC surface into logical domains. Each
// method requires one or more (entity, action) pairs, so operators can mint
// least-privilege macaroons scoped to a single domain — read-only fee data, a
// swap-only token — instead of the all-or-nothing token a single entity would
// force. Modeled on lnd's per-entity permission map.
const (
	// entityInfo covers daemon and wallet status plus the seed/unlock
	// lifecycle.
	entityInfo = "info"

	// entityVTXO covers VTXO inventory, in-round sends, refreshes, and
	// forfeit signing.
	entityVTXO = "vtxo"

	// entityAddress covers receive scripts, addresses, and receive-auth
	// key material.
	entityAddress = "address"

	// entityOnChain covers boarding, on-chain sends, sweeps, and
	// unilateral exit.
	entityOnChain = "onchain"

	// entityOOR covers out-of-round send sessions.
	entityOOR = "oor"

	// entityRound covers round participation and round queries.
	entityRound = "round"

	// entitySwap covers the swap subsystem.
	entitySwap = "swap"

	// entityRecovery covers vHTLC on-chain recovery jobs.
	entityRecovery = "recovery"

	// entityFees covers operator fee estimation and history.
	entityFees = "fees"

	// entityActivity covers the unified ledger, transaction history, and
	// activity inspection.
	entityActivity = "activity"
)

// darepodEntities is the full set of logical macaroon entities. The read-only
// macaroon grants read on each of them, and every method's required ops must
// name one of these entities.
var darepodEntities = []string{
	entityInfo,
	entityVTXO,
	entityAddress,
	entityOnChain,
	entityOOR,
	entityRound,
	entitySwap,
	entityRecovery,
	entityFees,
	entityActivity,
}

var darepodRPCPermissions = newDarepodRPCPermissions()

// newDarepodRPCPermissions returns the local daemon's explicit macaroon policy.
// Methods are grouped by logical entity and action so the taxonomy — which
// domain each call belongs to — is legible at a glance.
func newDarepodRPCPermissions() map[string][]bakery.Op {
	permissions := make(map[string][]bakery.Op)

	// grant records that every listed method of service requires the given
	// action on entity. Each method gets its own fresh op slice so callers
	// never alias a shared backing array.
	grant := func(service, entity, action string, methods ...string) {
		for _, method := range methods {
			fullMethod := "/" + service + "/" + method
			permissions[fullMethod] = []bakery.Op{{
				Entity: entity,
				Action: action,
			}}
		}
	}

	daemon := daemonrpc.DaemonService_ServiceDesc.ServiceName
	grant(daemon, entityInfo, "read",
		"GetInfo", "GetBalance",
	)
	grant(
		daemon, entityInfo, "write", "GenSeed", "InitWallet",
		"UnlockWallet",
	)
	grant(
		daemon, entityVTXO, "read", "ListVTXOs",
		"GetIndexedVTXOByPkScript", "GetVTXOExpiryInfo",
		"ListPendingForfeitParticipantSignatureRequests",
	)
	grant(
		daemon, entityVTXO, "write", "SendVTXO", "SignVTXOForfeit",
		"RefreshVTXOs", "RefreshCustomVTXOs",
		"SubmitForfeitParticipantSignatures", "LeaveVTXOs",
	)
	grant(
		daemon, entityAddress, "write", "NewAddress",
		"NewReceiveScript", "ReceiveAuthKey", "SignReceiveAuthMessage",
		"SignReceiveAuthMessageCompact", "ReceiveAuthECDH",
	)
	grant(
		daemon, entityOOR, "read", "GetIndexedOORSessionByTxid",
		"ListOORSessions", "GetOORSession",
	)
	grant(
		daemon, entityOOR, "write", "SendOOR", "PrepareOOR",
		"SignOORCustomInput",
	)
	grant(
		daemon, entityOnChain, "read", "ListBoardingSweeps",
		"GetUnrollStatus",
	)
	grant(
		daemon, entityOnChain, "write", "SendOnChain", "Board",
		"SweepBoardingUTXOs", "Unroll",
	)
	grant(
		daemon, entityRound, "read", "ListRounds", "GetRound",
		"WatchRounds",
	)
	grant(daemon, entityRound, "write",
		"JoinNextRound",
	)
	grant(daemon, entityFees, "read",
		"EstimateFee", "GetFeeHistory",
	)
	grant(daemon, entityActivity, "read",
		"ListTransactions",
	)
	grant(
		daemon, entityRecovery, "read", "GetVHTLCRecoveryStatus",
		"ListVHTLCRecoveries",
	)
	grant(
		daemon, entityRecovery, "write", "ArmVHTLCRecovery",
		"EscalateVHTLCRecovery", "CancelVHTLCRecovery",
	)

	swap := swapclientrpc.SwapClientService_ServiceDesc.ServiceName
	grant(
		swap, entitySwap, "read", "QuotePay", "ListSwaps", "GetSwap",
		"SubscribeSwaps", "ListCredits",
	)
	grant(
		swap, entitySwap, "write", "StartPay", "StartReceive",
		"ResumeSwap", "CreateCredit", "RedeemCredit",
	)

	walletdk := walletdkrpc.WalletService_ServiceDesc.ServiceName
	grant(
		walletdk, entityInfo, "read", "Balance", "Status",
		"SubscribeWallet",
	)
	grant(walletdk, entityInfo, "write",
		"Create", "Unlock",
	)
	grant(walletdk, entityActivity, "read",
		"List",
	)
	grant(walletdk, entityAddress, "write",
		"Recv",
	)
	grant(
		walletdk, entityOnChain, "read", "GetExitPlan", "ExitStatus",
		"ExitSummary",
	)
	grant(
		walletdk, entityOnChain, "write", "PrepareSend", "Send",
		"Deposit", "SweepWallet", "Exit",
	)

	inspect := walletdkrpc.WalletInspectionService_ServiceDesc.ServiceName
	grant(inspect, entityActivity, "read",
		"InspectActivity",
	)

	grant("walletrpc.VersionService", entityInfo, "read",
		"Version",
	)
	grant(
		"walletrpc.WalletService", entityInfo, "read", "Ping",
		"Network",
	)
	grant(
		"walletrpc.WalletService", entityInfo, "write",
		"ChangePassphrase",
	)
	grant(
		"walletrpc.WalletService", entityActivity, "read",
		"GetTransactions", "TransactionNotifications",
	)
	grant(
		"walletrpc.WalletService", entityAddress, "write",
		"NextAddress",
	)
	grant(
		"walletrpc.WalletService", entityOnChain, "read",
		"AccountNumber", "Accounts", "Balance",
		"SpentnessNotifications", "AccountNotifications",
	)
	grant(
		"walletrpc.WalletService", entityOnChain, "write",
		"RenameAccount", "NextAccount", "ImportPrivateKey",
		"FundTransaction", "SignTransaction", "PublishTransaction",
	)

	return permissions
}

// darepodReadOnlyPermissions returns the read op for every logical entity. A
// macaroon baked with these ops can invoke every read method across the daemon
// surface but no mutating method — the daemon's equivalent of lnd's
// readonly.macaroon.
func darepodReadOnlyPermissions() []bakery.Op {
	ops := make([]bakery.Op, 0, len(darepodEntities))
	for _, entity := range darepodEntities {
		ops = append(ops, bakery.Op{
			Entity: entity,
			Action: "read",
		})
	}

	return ops
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
