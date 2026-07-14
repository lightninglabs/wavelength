//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/credit"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// fakeCreditRegistry is a minimal actor.ActorRef stand-in for the credit
// registry used by router tests. It records admission requests and resolves
// every Ask with a configurable response.
type fakeCreditRegistry struct {
	payCalls int
	lastPay  *credit.StartCreditPayRequest

	receiveCalls int
	lastReceive  *credit.StartCreditReceiveRequest

	listCalls int
	listResp  *credit.ListCreditOpsResponse

	resp        credit.CreditResp
	receiveResp *credit.StartCreditResponse
	err         error
}

// Compile-time check that the fake satisfies the credit registry ref shape.
var _ actor.ActorRef[credit.CreditMsg, credit.CreditResp] = (*fakeCreditRegistry)(nil)

// ID returns a stable identifier.
func (f *fakeCreditRegistry) ID() string { return "fake-credit-registry" }

// Tell records nothing and always succeeds.
func (f *fakeCreditRegistry) Tell(context.Context, credit.CreditMsg) error {
	return nil
}

// Ask records pay admissions and resolves with the configured response.
func (f *fakeCreditRegistry) Ask(_ context.Context,
	msg credit.CreditMsg) actor.Future[credit.CreditResp] {

	switch m := msg.(type) {
	case *credit.StartCreditPayRequest:
		f.payCalls++
		f.lastPay = m

	case *credit.StartCreditReceiveRequest:
		f.receiveCalls++
		f.lastReceive = m

	case *credit.ListCreditOpsRequest:
		f.listCalls++
	}

	promise := actor.NewPromise[credit.CreditResp]()
	if f.err != nil {
		promise.Complete(fn.Err[credit.CreditResp](f.err))

		return promise.Future()
	}

	var resp credit.CreditResp = f.resp
	switch msg.(type) {
	case *credit.StartCreditReceiveRequest:
		if f.receiveResp != nil {
			resp = f.receiveResp
		}

	case *credit.ListCreditOpsRequest:
		if f.listResp != nil {
			resp = f.listResp
		} else {
			resp = &credit.ListCreditOpsResponse{}
		}
	}
	if resp == nil {
		resp = &credit.StartCreditResponse{OpID: "op-fake"}
	}
	promise.Complete(fn.Ok(resp))

	return promise.Future()
}
