package client

// Stats is a credential-free admission snapshot. Its fields are sampled
// independently, so a concurrent handoff can change either immediately.
// These gauges are not connection counts or a whole-process memory bound.
type Stats struct {
	ActiveReads int
	QueuedReads int
}

// Stats returns current admission occupancy without network I/O or callbacks.
// ActiveReads includes Open, reads and cleanup; queued calls hold no SMB session.
func (c *Client) Stats() Stats {
	return Stats{ActiveReads: len(c.slots), QueuedReads: len(c.waiters)}
}
