package daemon

// The external integration test wires the real hub without a production import.
var NewTestHub func() Hub
