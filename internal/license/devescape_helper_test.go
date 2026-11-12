package license

import "testing"

// enableDevEscapeForTest simulates the license_dev build posture for tests
// that exercise verify_mode=off behavior (unsigned token loading, off-mode
// mode-matrix assertions). It restores the compiled-in value afterwards, so
// gate regression tests (TestConfigureOffIgnoredInProductionBuild) still see
// the real production value when they pin it explicitly.
func enableDevEscapeForTest(t *testing.T) {
	t.Helper()
	orig := devEscapeHatchAllowed
	devEscapeHatchAllowed = true
	t.Cleanup(func() { devEscapeHatchAllowed = orig })
}
