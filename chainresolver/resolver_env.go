package chainresolver

// ResolverEnvironment holds the dependencies available to the resolver FSM
// during state transitions. It implements the protofsm.Environment interface.
type ResolverEnvironment struct {
	// name identifies this FSM instance (typically the VTXO outpoint
	// string).
	name string

	// Ctx holds the immutable VTXO metadata and OOR packages needed by
	// the resolver during state transitions.
	Ctx *ResolverContext
}

// Name returns the unique identifier for this FSM instance.
func (e *ResolverEnvironment) Name() string {
	return e.name
}

// NewResolverEnvironment creates a new resolver environment with the provided
// dependencies.
func NewResolverEnvironment(name string,
	ctx *ResolverContext) *ResolverEnvironment {

	return &ResolverEnvironment{
		name: name,
		Ctx:  ctx,
	}
}
