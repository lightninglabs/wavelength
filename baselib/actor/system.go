package actor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// stoppable defines an interface for components that can be stopped.
// This is unexported as it's an internal detail of ActorSystem for managing
// actors that need to be shut down.
type stoppable interface {
	Stop()
}

// SystemConfig holds configuration parameters for the ActorSystem.
type SystemConfig struct {
	// MailboxCapacity is the default capacity for actor mailboxes.
	MailboxCapacity int
}

// DefaultConfig returns a default configuration for the ActorSystem.
func DefaultConfig() SystemConfig {
	return SystemConfig{
		MailboxCapacity: 100,
	}
}

// ActorSystem manages the lifecycle of actors and provides coordination
// services such as a receptionist for actor discovery and a dead letter office
// for undeliverable messages. It also handles the graceful shutdown of all
// managed actors.
type ActorSystem struct {
	// receptionist is used for actor discovery.
	receptionist *Receptionist

	// actors stores all actors managed by the system, keyed by their ID.
	// This includes the deadLetterActor.
	actors map[string]stoppable

	// deadLetterActor handles undeliverable messages.
	deadLetterActor ActorRef[Message, any]

	// config holds the system-wide configuration.
	config SystemConfig

	// mu protects the 'actors' map.
	mu sync.RWMutex

	// ctx is the main context for the actor system.
	ctx context.Context

	// cancel cancels the main system context.
	cancel context.CancelFunc

	// actorWg tracks running actor goroutines for deterministic shutdown.
	actorWg sync.WaitGroup
}

// NewActorSystem creates a new actor system using the default configuration.
func NewActorSystem() *ActorSystem {
	return NewActorSystemWithConfig(DefaultConfig())
}

// NewActorSystemWithConfig creates a new actor system with custom configuration
func NewActorSystemWithConfig(config SystemConfig) *ActorSystem {
	ctx, cancel := context.WithCancel(context.Background())

	// Initialize the core ActorSystem components.
	system := &ActorSystem{
		receptionist: newReceptionist(),
		config:       config,
		actors:       make(map[string]stoppable),
		ctx:          ctx,
		cancel:       cancel,
	}

	// Define the behavior for the dead letter actor. It simply returns an
	// error indicating the message was undeliverable.
	deadLetterBehavior := NewFunctionBehavior(
		func(ctx context.Context, msg Message) fn.Result[any] {
			return fn.Err[any](errors.New(
				"message undeliverable: " + msg.MessageType(),
			))
		},
	)

	// Create the raw dead letter actor (*Actor instance). The DLO's own DLO
	// reference is nil to prevent loops if messages to the DLO itself fail.
	deadLetterActorCfg := ActorConfig[Message, any]{
		ID:          "dead-letters",
		Behavior:    deadLetterBehavior,
		DLO:         nil,
		MailboxSize: config.MailboxCapacity,
		Wg:          &system.actorWg,
	}
	deadLetterRawActor := NewActor[Message, any](deadLetterActorCfg)
	deadLetterRawActor.Start()
	system.deadLetterActor = deadLetterRawActor.Ref()

	// Add the raw actor to the map of stoppable actors. No lock needed here
	// as 'system' is not yet accessible concurrently.
	system.actors[deadLetterRawActor.id] = deadLetterRawActor

	// The system is now fully initialized and ready.
	return system
}

// newStoppedActorRef creates a stopped actor reference with the given ID.
// This is used to return a safe non-nil reference when actor creation fails,
// ensuring any calls to the returned ref will fail with ErrActorTerminated
// rather than causing a nil pointer panic.
func newStoppedActorRef[M Message, R any](id string) ActorRef[M, R] {
	cfg := ActorConfig[M, R]{ID: id}
	actor := NewActor(cfg)
	actor.Stop()
	return actor.Ref()
}

// RegisterWithSystem creates an actor with the given ID, service key, and
// behavior within the specified ActorSystem. It starts the actor, adds it to
// the system's management, registers it with the receptionist using the
// provided key, and returns its ActorRef.
func RegisterWithSystem[M Message, R any](as *ActorSystem, id string, key ServiceKey[M, R],
	behavior ActorBehavior[M, R],
) ActorRef[M, R] {

	if as.ctx.Err() != nil {
		// To avoid returning nil and causing a panic, we can create and
		// return a reference to a dummy actor that is already stopped.
		// This ensures that any calls to the returned ref will fail
		// with ErrActorTerminated.
		return newStoppedActorRef[M, R](id)
	}

	actorCfg := ActorConfig[M, R]{
		ID:          id,
		Behavior:    behavior,
		DLO:         as.deadLetterActor,
		MailboxSize: as.config.MailboxCapacity,
		Wg:          &as.actorWg,
	}
	actorInstance := NewActor(actorCfg)
	actorInstance.Start()

	// Add the actor instance to the system's list of stoppable actors.
	// This map is protected by the system's mutex.
	as.mu.Lock()
	as.actors[actorInstance.id] = actorInstance
	as.mu.Unlock()

	// Register the actor's reference with the receptionist under the given
	// service key, making it discoverable by other parts of the system.
	err := RegisterWithReceptionist(as.receptionist, key, actorInstance.Ref())
	if err != nil {
		// Type mismatch detected. Stop the actor we just created and
		// return a dummy stopped actor to avoid nil panic.
		actorInstance.Stop()
		as.mu.Lock()
		delete(as.actors, actorInstance.id)
		as.mu.Unlock()

		return newStoppedActorRef[M, R](id)
	}

	log.DebugS(as.ctx, "Actor registered with system",
		"actor_id", id,
		"service_key", key.name)

	return actorInstance.Ref()
}

// Receptionist returns the system's receptionist, which can be used for
// actor service discovery (finding actors by ServiceKey).
func (as *ActorSystem) Receptionist() *Receptionist {
	return as.receptionist
}

// DeadLetters returns a reference to the system's dead letter actor. Messages
// that cannot be delivered to their intended recipient (e.g., if an Ask
// context is cancelled before enqueuing) may be routed here if not otherwise
// handled.
func (as *ActorSystem) DeadLetters() ActorRef[Message, any] {
	return as.deadLetterActor
}

// Shutdown gracefully stops the actor system and waits for all actors to
// finish processing. It iterates through all managed actors, calls their Stop
// method, and then blocks until all actor goroutines have exited or the
// provided context expires. This ensures deterministic shutdown with guaranteed
// resource cleanup. This method is safe for concurrent use.
func (as *ActorSystem) Shutdown(ctx context.Context) error {
	// Cancel the main system context first to prevent new actor
	// registrations. Any RegisterWithSystem call that occurs after this
	// point will see as.ctx.Err() != nil and return a dummy stopped actor.
	// This ordering is critical to prevent a race where a new actor could
	// be registered and increment the WaitGroup after we snapshot but
	// before we wait, causing indefinite blocking.
	as.cancel()

	// Create a slice of actors to stop. This avoids holding the lock while
	// calling Stop() on each actor, and includes the dead letter actor.
	var actorsToStop []stoppable
	as.mu.RLock()
	for _, actor := range as.actors {
		actorsToStop = append(actorsToStop, actor)
	}
	as.mu.RUnlock()

	log.InfoS(ctx, "Actor system shutting down",
		"num_actors", len(actorsToStop))

	// Notify all managed actors to stop. Actor.Stop() is non-blocking.
	// Each actor's Stop method will cancel its internal context, leading
	// to the termination of its processing goroutine.
	for _, actor := range actorsToStop {
		actor.Stop()
	}

	// Clear the actors map after initiating their shutdown.
	as.mu.Lock()
	as.actors = nil
	as.mu.Unlock()

	// Wait for all actor goroutines to exit. We launch a goroutine to wait
	// on the WaitGroup so we can also respect the context deadline. If the
	// context times out, this goroutine continues running until the
	// WaitGroup reaches zero (which could be indefinite if actors are truly
	// hung). This is acceptable since shutdown timeouts indicate abnormal
	// conditions and the single goroutine overhead is negligible compared
	// to potentially leaked actor goroutines.
	done := make(chan struct{})
	go func() {
		as.actorWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All actors have finished processing.
		log.InfoS(ctx, "Actor system shutdown completed")

		return nil

	case <-ctx.Done():
		// Context expired before all actors finished—some goroutines
		// are still running and may leak. This indicates either
		// misbehaving actors or insufficient shutdown timeout.
		log.ErrorS(ctx, "Actor system shutdown incomplete, "+
			"some actors may have leaked", ctx.Err())

		return ctx.Err()
	}
}

// StopAndRemoveActor stops a specific actor by its ID and removes it from the
// ActorSystem's management. It returns true if the actor was found and stopped,
// false otherwise.
func (as *ActorSystem) StopAndRemoveActor(id string) bool {
	as.mu.Lock()
	defer as.mu.Unlock()

	actorToStop, exists := as.actors[id]
	if !exists {
		return false
	}

	// Stop the actor. This is non-blocking.
	actorToStop.Stop()

	// Remove from the system's management.
	delete(as.actors, id)

	log.DebugS(as.ctx, "Actor stopped and removed from system",
		"actor_id", id)

	return true
}

// UnregisterFromReceptionist removes an actor reference from a service key in
// the given receptionist. It returns true if the reference was found and
// removed, and false otherwise. This is a package-level generic function
// because methods cannot have their own type parameters in Go.
func UnregisterFromReceptionist[M Message, R any](r *Receptionist,
	key ServiceKey[M, R], refToRemove ActorRef[M, R]) bool {

	r.mu.Lock()
	defer r.mu.Unlock()

	refs, exists := r.registrations[key.name]
	if !exists {
		return false
	}

	found := false

	// Build a new slice containing only the references that are not the one
	// to be removed.
	newRefs := make([]any, 0, len(refs)-1) // Pre-allocate assuming one removal
	for _, itemInSlice := range refs {     // itemInSlice is of type 'any'
		// Try to assert the item from the slice to the specific
		// ActorRef[M,R] type we are trying to remove.
		if specificActorRef, ok := itemInSlice.(ActorRef[M, R]); ok {
			// If the type assertion is successful and it's the one
			// we want to remove, mark as found and skip adding it
			// to newRefs.
			if specificActorRef == refToRemove {
				found = true
				continue // Don't add to newRefs, effectively removing it.
			}
		}
		newRefs = append(newRefs, itemInSlice)
	}

	if !found {
		return false
	}

	// If the new list of references is empty, remove the key from the map
	// and clean up the type registry. This prevents memory leaks and allows
	// re-registration with different types after all actors are unregistered.
	if len(newRefs) == 0 {
		delete(r.registrations, key.name)
		delete(r.typeRegistry, key.name)
	} else {
		r.registrations[key.name] = newRefs
	}

	return true
}

// ServiceKey is a type-safe identifier used for registering and discovering
// actors via the Receptionist. The generic type parameters M (Message) and R
// (Response) ensure that only actors handling compatible message/response types
// are associated with and retrieved for this key.
type ServiceKey[M Message, R any] struct {
	name string
}

// NewServiceKey creates a new service key with the given name. The name is used
// as the lookup key within the Receptionist.
func NewServiceKey[M Message, R any](name string) ServiceKey[M, R] {
	return ServiceKey[M, R]{name: name}
}

// Spawn registers an actor for this service key within the given ActorSystem.
// It's a convenience method that calls RegisterWithSystem, starting the actor
// and registering it with the receptionist.
func (sk ServiceKey[M, R]) Spawn(as *ActorSystem, id string,
	behavior ActorBehavior[M, R]) ActorRef[M, R] {

	return RegisterWithSystem(as, id, sk, behavior)
}

// Unregister removes an actor reference associated with this service key from
// the ActorSystem's receptionist. The actor continues running and can still be
// accessed through other service keys it may be registered under. To stop the
// actor, use StopAndRemoveActor separately. This separation allows actors to
// provide multiple services and gracefully degrade by stopping advertisement
// on some interfaces while continuing to serve others.
//
// Returns true if the actor was found and unregistered, false otherwise.
func (sk ServiceKey[M, R]) Unregister(as *ActorSystem,
	refToRemove ActorRef[M, R]) bool {

	return UnregisterFromReceptionist(
		as.Receptionist(), sk, refToRemove,
	)
}

// UnregisterAll removes all actor references associated with this service key
// from the ActorSystem's receptionist. The actors continue running and can
// still be accessed through other service keys. To stop the actors, use
// StopAndRemoveActor separately for each actor reference.
//
// Returns the number of actors that were unregistered.
func (sk ServiceKey[M, R]) UnregisterAll(as *ActorSystem) int {
	r := as.Receptionist()

	r.mu.Lock()
	defer r.mu.Unlock()

	currentRefs, exists := r.registrations[sk.name]
	if !exists {
		return 0
	}

	// Build a new slice containing only references that don't match our
	// service key's type. This handles the case where the same key name
	// might have refs of different types registered.
	newRefs := make([]any, 0, len(currentRefs))
	unregisteredCount := 0

	for _, item := range currentRefs {
		if _, ok := item.(ActorRef[M, R]); ok {
			// This ref matches our type, so we're unregistering it.
			unregisteredCount++
		} else {
			// Different type, keep it.
			newRefs = append(newRefs, item)
		}
	}

	if unregisteredCount == 0 {
		return 0
	}

	// Update or delete the registration entry. If all refs are removed,
	// also clean up the type registry to prevent memory leaks and allow
	// re-registration with different types.
	if len(newRefs) == 0 {
		delete(r.registrations, sk.name)
		delete(r.typeRegistry, sk.name)
	} else {
		r.registrations[sk.name] = newRefs
	}

	return unregisteredCount
}

// serviceTypeInfo captures the type signature of a service for validation.
type serviceTypeInfo struct {
	msgTypeName  string
	respTypeName string
}

// Receptionist provides service discovery for actors. Actors can be registered
// under a ServiceKey and later discovered by other actors or system components.
type Receptionist struct {
	// registrations stores ActorRef instances, keyed by ServiceKey.name.
	registrations map[string][]any

	// typeRegistry tracks the types registered under each service name to
	// prevent type conflicts.
	typeRegistry map[string]serviceTypeInfo

	// mu protects access to registrations and typeRegistry.
	mu sync.RWMutex
}

// newReceptionist creates a new Receptionist instance.
func newReceptionist() *Receptionist {
	return &Receptionist{
		registrations: make(map[string][]any),
		typeRegistry:  make(map[string]serviceTypeInfo),
	}
}

// RegisterWithReceptionist registers an actor with a service key in the given
// receptionist. This is a package-level generic function because methods
// cannot have their own type parameters in Go (as of the current version).
// It validates that the service key types match any existing registrations
// under the same name and returns an error if there's a type mismatch.
func RegisterWithReceptionist[M Message, R any](
	r *Receptionist, key ServiceKey[M, R], ref ActorRef[M, R]) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	// Get type names for validation using reflect to avoid allocating
	// zero-value instances. This is more efficient and idiomatic for
	// extracting type information from generic type parameters.
	msgTypeName := reflect.TypeOf((*M)(nil)).Elem().String()
	respTypeName := reflect.TypeOf((*R)(nil)).Elem().String()

	expectedTypes := serviceTypeInfo{
		msgTypeName:  msgTypeName,
		respTypeName: respTypeName,
	}

	// Check if this service name is already registered with different types.
	if existingTypes, exists := r.typeRegistry[key.name]; exists {
		if existingTypes != expectedTypes {
			return fmt.Errorf("%w: service '%s' already registered "+
				"with types (%s, %s), cannot register with (%s, %s)",
				ErrServiceKeyTypeMismatch, key.name,
				existingTypes.msgTypeName, existingTypes.respTypeName,
				msgTypeName, respTypeName)
		}
	} else {
		// First registration for this name, record the types.
		r.typeRegistry[key.name] = expectedTypes
	}

	// Initialize the slice for this key if it's the first registration.
	if _, exists := r.registrations[key.name]; !exists {
		r.registrations[key.name] = make([]interface{}, 0)
	}

	r.registrations[key.name] = append(r.registrations[key.name], ref)

	return nil
}

// FindInReceptionist returns all actors registered with a service key in the
// given receptionist. This is a package-level generic function because methods
// cannot have their own type parameters. It performs a type assertion to ensure
// that only ActorRefs matching the ServiceKey's generic types (M, R) are
// returned, providing type safety.
func FindInReceptionist[M Message, R any](
	r *Receptionist, key ServiceKey[M, R]) []ActorRef[M, R] {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if refs, exists := r.registrations[key.name]; exists {
		typedRefs := make([]ActorRef[M, R], 0, len(refs))
		for _, ref := range refs {
			// Make sure that the reference is of the correct type.
			// This type assertion is crucial for type safety, ensuring
			// that the returned ActorRefs match the expected M and R.
			if typedRef, ok := ref.(ActorRef[M, R]); ok {
				typedRefs = append(typedRefs, typedRef)
			}
		}
		return typedRefs
	}

	return nil
}
