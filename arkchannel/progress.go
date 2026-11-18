package arkchannel

// subscribeChannelChanges provides a coalesced wake-up after durable progress.
// Subscribe before reading the record, and always reload it on wake-up. The
// signal is only a latency optimization; callers still need a replay timer.
func (c *Coordinator) subscribeChannelChanges(id ID) (<-chan struct{}, func()) {
	changed := make(chan struct{}, 1)
	c.changesMu.Lock()
	if c.changes == nil {
		c.changes = make(map[ID]map[chan struct{}]struct{})
	}
	if c.changes[id] == nil {
		c.changes[id] = make(map[chan struct{}]struct{})
	}
	c.changes[id][changed] = struct{}{}
	c.changesMu.Unlock()

	return changed, func() {
		c.changesMu.Lock()
		defer c.changesMu.Unlock()

		delete(c.changes[id], changed)
		if len(c.changes[id]) == 0 {
			delete(c.changes, id)
		}
	}
}

// notifyChannelChanged never blocks a durable transition or creates work for
// an unchanged record. Slow listeners need only one signal to reload state.
func (c *Coordinator) notifyChannelChanged(id ID) {
	c.changesMu.Lock()
	defer c.changesMu.Unlock()

	for changed := range c.changes[id] {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}
