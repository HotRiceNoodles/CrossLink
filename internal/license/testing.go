package license

// SetGlobalForTest sets the global license gate for testing purposes.
// This allows integration tests in other packages to simulate a specific license tier
// without going through the full Init() flow.
func SetGlobalForTest(g *Gate) {
	global = g
}
