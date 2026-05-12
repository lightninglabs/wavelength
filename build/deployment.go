package build

// DeploymentType is an enum specifying the deployment type of this binary.
type DeploymentType byte

const (
	// Development is a deployment in development mode. This offers less
	// stability and more logging.
	Development DeploymentType = iota

	// Production is a deployment in production mode. This offers
	// heightened stability and fewer logs.
	Production
)

// String returns a human-readable name for a DeploymentType.
func (d DeploymentType) String() string {
	switch d {
	case Development:
		return "development"

	case Production:
		return "production"

	default:
		return "unknown"
	}
}
