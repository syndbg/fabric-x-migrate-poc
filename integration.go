//go:build integration

package migrate

import (
	"testing"

	"github.com/syndbg/fabric-x-migrate-poc/internal/integrationtest"
)

const FabricVersion = integrationtest.Version

func CaptureFabricSnapshots(t *testing.T, repository, stateDB string, channels ...string) map[string]string {
	t.Helper()
	return integrationtest.Capture(t, repository, stateDB, channels...)
}
