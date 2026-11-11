package notifyrelay

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSQLAbsentRevokeConsumesProof keeps no-op management inside the same
// challenge/counter transaction as a revoke that removes a live route.
func TestSQLAbsentRevokeConsumesProof(t *testing.T) {
	h := newSQLAuthHarness(t)
	challenge := h.challenge(t)
	action := ManagementPayload{
		Action: "revoke_subscription", SubscriptionID: "absent",
	}
	result, err := h.store.applyManagement(
		t.Context(), challenge, 1, action, h.now,
	)
	require.NoError(t, err)
	require.Equal(t, ManagementResult{}, result)
	owner, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.EqualValues(t, 1, owner.Counter)
	_, err = h.database.Queries.GetConsumedChallenge(
		t.Context(), secretHash(challenge.ID),
	)
	require.NoError(t, err)
	_, err = h.store.applyManagement(
		t.Context(), challenge, 2, action, h.now,
	)
	require.ErrorIs(t, err, ErrReplay)
	owner, err = h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.EqualValues(t, 1, owner.Counter)
}

// TestRepeatedAndForeignRevokeHTTP preserves foreign ownership while making
// absent and foreign IDs indistinguishable through real signed HTTP actions.
func TestRepeatedAndForeignRevokeHTTP(t *testing.T) {
	first := newRelayHarness(t, nil)
	own := first.subscribe("own-route", false)
	second := newRelayHarness(t, first.store)
	foreign := second.subscribe("other-route", false)
	retained, err := first.store.database.Queries.GetSubscription(
		t.Context(), foreign.SubscriptionID,
	)
	require.NoError(t, err)
	response := first.manage(ManagementPayload{
		Action:         "revoke_subscription",
		SubscriptionID: own.SubscriptionID,
	})
	require.Equal(t, http.StatusOK, response.Code)
	expected := response.Body.String()
	absent, err := randomSecret(24)
	require.NoError(t, err)
	for _, id := range []string{
		own.SubscriptionID, foreign.SubscriptionID, absent,
	} {
		response = first.manage(ManagementPayload{
			Action: "revoke_subscription", SubscriptionID: id,
		})
		require.Equal(t, http.StatusOK, response.Code)
		require.JSONEq(t, expected, response.Body.String())
	}
	_, err = first.store.database.Queries.GetSubscription(
		t.Context(), own.SubscriptionID,
	)
	require.ErrorIs(t, err, sql.ErrNoRows)
	after, err := first.store.database.Queries.GetSubscription(
		t.Context(), foreign.SubscriptionID,
	)
	require.NoError(t, err)
	require.Equal(t, retained, after)
	caller, err := first.store.loadInstallation(t.Context(), first.ownerID)
	require.NoError(t, err)
	require.EqualValues(t, 5, caller.Counter)
	other, err := first.store.loadInstallation(t.Context(), second.ownerID)
	require.NoError(t, err)
	require.EqualValues(t, 1, other.Counter)
}
