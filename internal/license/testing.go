package license

// SetGlobalForTest sets the global license gate for testing purposes.
// This allows integration tests in other packages to simulate a specific license tier
// without going through the full Init() flow. Tier simulation implies the
// schema is present (U-01 guard is a production Init concern, not a test
// fixture concern) — tests that want the guard's degradation behavior
// construct Gates directly instead of going through this seam.
func SetGlobalForTest(g *Gate) {
	if g != nil {
		g.SetSchemaSupported(true)
	}
	global = g
}
