// Package protocol defines the public event, error, lifecycle, rendering, and
// stdin control contracts for one Runtime operation.
//
// ProcessOutput serializes every event through one renderer, emits exactly one
// terminal result, and rejects all later events. Successfully emitted warnings
// are snapshotted into a bounded, authoritative result summary.
//
// The contracttest subpackage validates raw NDJSON framing, envelopes, terminal
// semantics, and warning summaries for command integration tests.
package protocol
