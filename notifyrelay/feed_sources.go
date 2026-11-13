package notifyrelay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
)

// SourceConfig is publisher-owned connection and identity policy. No HTTP
// request chooses a source endpoint, credential path or subscriber key.
// Role is an app-facing label, such as operator or swap_server; it grants no
// authority. EventClasses limits the class mask offered to an installation.
type SourceConfig struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	Endpoint     string `json:"endpoint"`
	PublicKey    string `json:"public_key"`
	TLSCertFile  string `json:"tls_cert_file,omitempty"`
	MacaroonFile string `json:"macaroon_file,omitempty"`
	EventClasses uint32 `json:"event_classes"`
}

// feedSource retains one subscriber key per unrelated producer. The existing
// mailbox metadata proof is not source-bound, so that key must not be shared.
type feedSource struct {
	config     SourceConfig
	public     *btcec.PublicKey
	subscriber *btcec.PrivateKey
	mailboxID  string
}

// String excludes the persisted subscriber private key from diagnostics.
func (s *feedSource) String() string { return "notification feed source" }

// FeedOffer is publisher consent for a local route, not a wallet delegation.
// The app asks its wallet to authorize this exact
// source/key/route/class/expiry. Only the subscriber public key and its route
// signature leave the relay.
type FeedOffer struct {
	SourceID            string `json:"source_id"`
	Role                string `json:"role"`
	SourcePubkey        string `json:"source_pubkey"`
	SubscriberPubkey    string `json:"subscriber_pubkey"`
	GrantID             string `json:"grant_id"`
	EventClasses        uint32 `json:"event_classes"`
	ExpiresAtUnix       int64  `json:"expires_at_unix"`
	SubscriberSignature string `json:"subscriber_signature"`
}

// FeedBindingRequest returns a producer-issued token after wallet
// authorization. The signed management request selects a locally owned
// subscription and a configured source. The token never selects a different
// subscription.
type FeedBindingRequest struct {
	SourceID      string `json:"source_id"`
	FeedToken     string `json:"feed_token"`
	EventClasses  uint32 `json:"event_classes"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
}

// validSourceConfig rejects ambiguous connection/identity policy at startup.
func validSourceConfig(c SourceConfig) (*btcec.PublicKey, error) {
	if !validIdentifier(c.ID, 100) || !validIdentifier(c.Role, 100) ||
		c.EventClasses == 0 || c.EventClasses&^uint32(7) != 0 {
		return nil, errors.New("invalid feed source policy")
	}
	host, port, err := net.SplitHostPort(c.Endpoint)
	if err != nil || host == "" {
		return nil, errors.New("invalid feed source endpoint")
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return nil, errors.New("invalid feed source endpoint")
	}
	raw, err := hex.DecodeString(c.PublicKey)
	if err != nil {
		return nil, errors.New("invalid feed source identity")
	}
	if len(raw) != 33 || hex.EncodeToString(raw) != c.PublicKey {
		return nil, errors.New("invalid feed source identity")
	}
	public, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, errors.New("invalid feed source identity")
	}

	return public, nil
}

// restoreSources creates immutable startup policy and restores committed keys.
// Reusing a source ID for another producer fails rather than reusing its proof.
func (s *SQLStore) restoreSources(ctx context.Context,
	configs []SourceConfig) error {

	if len(configs) > 16 {
		return errors.New("too many feed sources")
	}
	sources := make(map[string]*feedSource, len(configs))
	for _, c := range configs {
		public, err := validSourceConfig(c)
		if err != nil {
			return err
		}
		if _, exists := sources[c.ID]; exists {
			return errors.New("duplicate feed source")
		}
		sources[c.ID] = &feedSource{config: c, public: public}
	}
	for _, source := range sources {
		if err := s.restoreSourceKey(ctx, source); err != nil {
			return err
		}
	}
	s.sources = sources

	return nil
}

// restoreSourceKey uses the retained database identity even when another
// replica initialized it first. Candidate keys never replace saved authority.
func (s *SQLStore) restoreSourceKey(ctx context.Context,
	source *feedSource) error {

	candidate, err := btcec.NewPrivateKey()
	if err != nil {
		return err
	}

	return s.database.Write(ctx, func(q *sqlc.Queries) error {
		if _, err := q.InsertSource(ctx, sqlc.InsertSourceParams{
			ID:            source.config.ID,
			SourcePubkey:  source.public.SerializeCompressed(),
			SubscriberKey: candidate.Serialize(),
		}); err != nil {
			return err
		}
		saved, err := q.GetSource(ctx, source.config.ID)
		if err != nil {
			return err
		}
		if !bytes.Equal(
			saved.SourcePubkey, source.public.SerializeCompressed(),
		) {
			return errors.New("feed source identity changed")
		}
		key, _ := btcec.PrivKeyFromBytes(saved.SubscriberKey)
		if key.Key.IsZero() || !bytes.Equal(
			key.Serialize(), saved.SubscriberKey,
		) {
			return errors.New("invalid saved subscriber key")
		}
		mailboxID, err := notification.FeedMailboxID(
			key.PubKey().SerializeCompressed(),
		)
		if err != nil {
			return err
		}
		source.subscriber = key
		source.mailboxID = mailboxID

		return nil
	})
}

// feedOffers signs only publisher-admitted routes. Sorting makes retries
// deterministic without revealing a wallet identity or private subscriber key.
func (s *SQLStore) feedOffers(ctx context.Context, q *sqlc.Queries,
	subscriptionID string) ([]FeedOffer, error) {

	sub, err := q.GetSubscription(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(s.sources))
	for id := range s.sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	offers := make([]FeedOffer, 0, len(ids))
	for _, id := range ids {
		source := s.sources[id]
		classes := source.config.EventClasses
		if !sub.AlertsEnabled {
			classes &^= notification.FeedClassPayment
		}
		if classes == 0 {
			continue
		}
		subscriber := source.subscriber.PubKey().SerializeCompressed()
		message, err := notification.FeedRouteSigningMessage(
			source.public.SerializeCompressed(), subscriber, sub.ID,
			classes, sub.ExpiresAt,
		)
		if err != nil {
			return nil, err
		}
		digest := chainhash.TaggedHash(
			[]byte(notification.FeedRouteSignatureTag), message,
		)
		signature, err := schnorr.Sign(source.subscriber, digest[:])
		if err != nil {
			return nil, err
		}
		encodedSignature := base64.RawURLEncoding.EncodeToString(
			signature.Serialize(),
		)
		offers = append(offers, FeedOffer{
			SourceID: id, Role: source.config.Role,
			SourcePubkey:        source.config.PublicKey,
			SubscriberPubkey:    hex.EncodeToString(subscriber),
			GrantID:             sub.ID,
			EventClasses:        classes,
			ExpiresAtUnix:       sub.ExpiresAt,
			SubscriberSignature: encodedSignature,
		})
	}

	return offers, nil
}

// bindFeedTx binds non-authorizing correlation only to a locally owned route.
// There is deliberately no global token uniqueness constraint: a device that
// learns another route's token must not be able to poison that route's mapping.
func (s *SQLStore) bindFeedTx(ctx context.Context, q *sqlc.Queries, owner,
	id string, request *FeedBindingRequest, now time.Time) (
	ManagementResult, error) {

	if request == nil {
		return ManagementResult{}, ErrNotFound
	}
	source, configured := s.sources[request.SourceID]
	if !configured {
		return ManagementResult{}, ErrNotFound
	}
	sub, err := q.GetSubscription(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagementResult{}, ErrNotFound
	}
	if err != nil {
		return ManagementResult{}, err
	}
	if sub.InstallationID != owner || sub.ExpiresAt <= now.Unix() {
		return ManagementResult{}, ErrNotFound
	}
	classes := source.config.EventClasses
	if !sub.AlertsEnabled {
		classes &^= notification.FeedClassPayment
	}
	token, err := base64.RawURLEncoding.DecodeString(request.FeedToken)
	encodedToken := base64.RawURLEncoding.EncodeToString(token)
	if err != nil || len(token) != notification.FeedTokenSize ||
		encodedToken != request.FeedToken ||
		request.EventClasses == 0 ||
		request.EventClasses&^classes != 0 ||
		request.ExpiresAtUnix <= now.Unix() ||
		request.ExpiresAtUnix > sub.ExpiresAt {
		return ManagementResult{}, ErrNotFound
	}
	if err := q.PutFeedBinding(ctx, sqlc.PutFeedBindingParams{
		SourceID: source.config.ID, SubscriptionID: sub.ID,
		FeedToken:    token,
		EventClasses: int32(request.EventClasses),
		ExpiresAt:    request.ExpiresAtUnix,
	}); err != nil {
		return ManagementResult{}, err
	}

	return ManagementResult{
		SubscriptionID: sub.ID,
		ExpiresAt:      request.ExpiresAtUnix,
	}, nil
}
