package migrate

import (
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
)

// PostStepCallback is a Go function run once after the SQL of a given
// migration version has been applied, and before that version is marked as
// cleanly applied.
type PostStepCallback func() error

// postStepDriver wraps a migration database driver to run PostStepCallbacks
// at the same point in a migration step as the pre-fork golang-migrate hook.
//
// A migration step first marks the target version dirty, then runs the SQL,
// and finally marks the version clean. The wrapper runs the callback just
// before the final clean mark, so an error leaves the database dirty at that
// version, exactly as the removed upstream hook did. This does not depend on
// the Target in use, since every target drives the same step sequence.
type postStepDriver struct {
	database.Driver

	callbacks map[uint]PostStepCallback

	// stepFrom is the clean version recorded when the current step was
	// marked dirty, or -1 if the database had no version.
	stepFrom int

	// stepTo is the version of the step currently in flight, or -1.
	stepTo int
}

// newPostStepDriver wraps the driver when callbacks are configured and
// returns it unchanged otherwise.
func newPostStepDriver(drv database.Driver,
	callbacks map[uint]PostStepCallback) database.Driver {

	if len(callbacks) == 0 {
		return drv
	}

	return &postStepDriver{
		Driver:    drv,
		callbacks: callbacks,
		stepFrom:  -1,
		stepTo:    -1,
	}
}

// SetVersion records the start of a step when a version is marked dirty and
// runs the matching callback before the step is marked clean. Downward steps
// never run callbacks, since the callbacks only describe upward migrations.
func (d *postStepDriver) SetVersion(version int, dirty bool) error {
	if dirty {
		from, _, err := d.Driver.Version()
		if err != nil {
			return err
		}

		d.stepFrom, d.stepTo = from, version

		return d.Driver.SetVersion(version, dirty)
	}

	cb, ok := d.callbacks[uint(version)]
	if ok && d.stepTo == version && version > d.stepFrom {
		if err := cb(); err != nil {
			return fmt.Errorf("post-step callback for version "+
				"%d: %w", version, err)
		}
	}
	d.stepFrom, d.stepTo = -1, -1

	return d.Driver.SetVersion(version, dirty)
}
