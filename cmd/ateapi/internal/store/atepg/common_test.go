// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package atepg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/authz"
	"go.ate.dev/substrate/internal/installdefaults"
)

// clearTables deletes rows from every mutable Substrate and OpenFGA table in
// foreign-key order without allocating new relation files via TRUNCATE.
func clearTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	const stmt = `
		DELETE FROM tags;
		DELETE FROM actors;
		DELETE FROM actor_templates;
		DELETE FROM atespaces;
		DELETE FROM global_access_policy;
		DELETE FROM workers;
		DELETE FROM leases;
		DELETE FROM worker_outbox;
		DELETE FROM worker_outbox_trim;
		DELETE FROM tuple;
		DELETE FROM changelog`
	if _, err := pool.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("clearing tables: %v", err)
	}
}

func clearAll(t *testing.T, p *Persistence) {
	t.Helper()
	clearTables(t, p.pool)
}

func setupPostgresPersistence(t *testing.T) *Persistence {
	t.Helper()
	pool, dropDB := cloneTemplatePool(t)
	p := NewMigratedPersistence(pool)
	t.Cleanup(func() {
		p.Close()
		dropDB()
	})
	setTestPolicyManager(t, p)
	return p
}

// setTestPolicyManager gives p an OpenFGA-backed PolicyManager, as the server
// always does.
func setTestPolicyManager(t *testing.T, p *Persistence) {
	t.Helper()
	fgaServer, err := authz.NewOpenFGAServer(p.pool)
	if err != nil {
		t.Fatalf("NewOpenFGAServer failed: %v", err)
	}
	t.Cleanup(fgaServer.Close)
	_, policyManager, err := authz.New(t.Context(), p.pool, fgaServer, nil, authz.SystemPrincipals{
		ControllerSPIFFEID:       installdefaults.ControllerSPIFFEID(installdefaults.SystemNamespace),
		EgressGatewaySPIFFEIDs:   []string{installdefaults.EgressSPIFFEID(installdefaults.SystemNamespace)},
		IngressRouterSPIFFEID:    installdefaults.RouterSPIFFEID(installdefaults.SystemNamespace),
		AteletSPIFFEID:           installdefaults.AteletSPIFFEID(installdefaults.SystemNamespace),
		APIServerSidecarSPIFFEID: installdefaults.APIServerSPIFFEID(installdefaults.SystemNamespace),
	})
	if err != nil {
		t.Fatalf("authz.New failed: %v", err)
	}
	p.SetPolicyManager(policyManager)
}

func newTestAtespace(name string) *ateapi.Atespace {
	return &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: name}}
}

func createTestAtespace(t *testing.T, s *Persistence, name string) {
	t.Helper()
	if _, err := s.CreateAtespace(context.Background(), newTestAtespace(name)); err != nil {
		t.Fatalf("CreateAtespace(%q) failed: %v", name, err)
	}
}
