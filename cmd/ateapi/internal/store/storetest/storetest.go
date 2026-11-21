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

// Package storetest provides isolated PostgreSQL-backed stores for tests.
//
// One PostgreSQL container is shared by every test in a package; each test gets
// its own database. Nothing stops that container when the test binary exits, so
// packages using this package must call [Shutdown] from their TestMain.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openfga/openfga/pkg/server"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/authz"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/atepg"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/dockerenv"
	"go.ate.dev/substrate/internal/installdefaults"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	templateDatabaseName = "ateapi_template"
	testPoolMaxConns     = 4
)

type testDatabase struct {
	pool          *pgxpool.Pool
	fgaServer     *server.Server
	policyManager *authz.PolicyManager
}

var (
	containerOnce sync.Once
	adminPool     *pgxpool.Pool
	containerPG   *postgres.PostgresContainer
	containerErr  error
	// createDBMu serializes CREATE DATABASE ... TEMPLATE calls because
	// PostgreSQL rejects concurrent template reads with SQLSTATE 55006.
	createDBMu    sync.Mutex
	databaseCount atomic.Uint64
	freeDBMu      sync.Mutex
	freeDBs       []*testDatabase
	// pools maps each *atepg.Persistence SetupPostgresPersistence returned to
	// its pool, for fixtures the store API cannot write.
	pools sync.Map
)

// SetupTestStore returns a real PostgreSQL-backed store with a database unique
// to this test. A shared container keeps this suitable for packages that run
// subtests in parallel; databases are reset and recycled during cleanup.
func SetupTestStore(t *testing.T) (store.Interface, func()) {
	t.Helper()
	return SetupPostgresPersistence(t), func() {}
}

// SetupPostgresPersistence returns an isolated atepg persistence instance.
func SetupPostgresPersistence(t *testing.T) *atepg.Persistence {
	t.Helper()
	db := acquireTestDatabase(t)
	persistence := atepg.NewMigratedPersistence(db.pool)
	persistence.SetPolicyManager(db.policyManager)
	pools.Store(persistence, db.pool)
	t.Cleanup(func() {
		persistence.Close()
		pools.Delete(persistence)
		if err := resetTestDatabase(context.Background(), db.pool); err != nil {
			db.fgaServer.Close()
			db.pool.Close()
			t.Errorf("resetting PostgreSQL test database: %v", err)
			return
		}
		releaseTestDatabase(db)
	})
	return persistence
}

func acquireTestDatabase(t *testing.T) *testDatabase {
	t.Helper()
	admin := requireAdminPool(t)

	freeDBMu.Lock()
	if n := len(freeDBs); n > 0 {
		db := freeDBs[n-1]
		freeDBs = freeDBs[:n-1]
		freeDBMu.Unlock()
		return db
	}
	freeDBMu.Unlock()

	ctx := context.Background()
	databaseName := fmt.Sprintf("ateapi_test_%d", databaseCount.Add(1))
	createDBMu.Lock()
	_, createErr := admin.Exec(ctx, "CREATE DATABASE "+databaseName+" TEMPLATE "+templateDatabaseName)
	createDBMu.Unlock()
	if createErr != nil {
		t.Fatalf("creating PostgreSQL test database: %v", createErr)
	}

	config := admin.Config().Copy()
	config.ConnConfig.Database = databaseName
	config.MaxConns = testPoolMaxConns
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connecting to PostgreSQL test database: %v", err)
	}
	fgaServer, err := authz.NewOpenFGAServer(pool)
	if err != nil {
		pool.Close()
		t.Fatalf("creating OpenFGA server: %v", err)
	}
	_, policyManager, err := authz.New(ctx, pool, fgaServer, nil, DefaultSystemPrincipals())
	if err != nil {
		fgaServer.Close()
		pool.Close()
		t.Fatalf("initializing OpenFGA authz: %v", err)
	}
	return &testDatabase{
		pool:          pool,
		fgaServer:     fgaServer,
		policyManager: policyManager,
	}
}

func releaseTestDatabase(db *testDatabase) {
	freeDBMu.Lock()
	freeDBs = append(freeDBs, db)
	freeDBMu.Unlock()
}

func resetTestDatabase(ctx context.Context, pool *pgxpool.Pool) error {
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
		DELETE FROM changelog;
		DELETE FROM assertion`
	_, err := pool.Exec(ctx, stmt)
	return err
}

// DefaultSystemPrincipals returns valid SystemPrincipals for tests that
// initialize authz.New without testing system component roles.
func DefaultSystemPrincipals() authz.SystemPrincipals {
	return authz.SystemPrincipals{
		ControllerSPIFFEID:       installdefaults.ControllerSPIFFEID(installdefaults.SystemNamespace),
		EgressGatewaySPIFFEIDs:   []string{installdefaults.EgressSPIFFEID(installdefaults.SystemNamespace)},
		IngressRouterSPIFFEID:    installdefaults.RouterSPIFFEID(installdefaults.SystemNamespace),
		AteletSPIFFEID:           installdefaults.AteletSPIFFEID(installdefaults.SystemNamespace),
		APIServerSidecarSPIFFEID: installdefaults.APIServerSPIFFEID(installdefaults.SystemNamespace),
	}
}

// MustCreateAtespace creates name unless it already exists. PostgreSQL enforces
// the parent relationship for actor and snapshot records, so test fixtures use
// this before seeding those resources.
func MustCreateAtespace(t *testing.T, ctx context.Context, s store.Interface, name string) {
	t.Helper()
	_, err := s.CreateAtespace(ctx, &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: name}})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("creating test atespace %q: %v", name, err)
	}
}

// MustCreateActor ensures actor's parent atespace exists, then creates actor.
// Use the store method directly only in tests that exercise missing-parent
// behavior.
func MustCreateActor(t *testing.T, ctx context.Context, s store.Interface, actor *ateapi.Actor) *ateapi.Actor {
	t.Helper()
	atespace := actor.GetMetadata().GetAtespace()
	MustCreateAtespace(t, ctx, s, atespace)
	created, err := s.CreateActor(ctx, actor)
	if err != nil {
		t.Fatalf("creating test actor %q/%q: %v", atespace, actor.GetMetadata().GetName(), err)
	}
	return created
}

// MustSeedAssignment books assignment on workerName as BindActorToWorker does,
// stamping its metadata the same way, but without the Actor update
// BindActorToWorker commits with it. Tests use it to seed an Actor already
// placed, or a Worker's bookings with no Actor stored behind them. s must come
// from SetupPostgresPersistence or SetupTestStore.
func MustSeedAssignment(t *testing.T, ctx context.Context, s store.Interface, workerName string, assignment *ateapi.ActorAssignment) {
	t.Helper()
	persistence, _ := s.(*atepg.Persistence)
	pool, ok := pools.Load(persistence)
	if !ok {
		t.Fatalf("seeding an assignment: store %T was not made by SetupPostgresPersistence", s)
	}
	now := timestamppb.Now()
	assignment.Metadata = &ateapi.ResourceMetadata{
		Name:       assignment.GetActorUid(),
		Uid:        uuid.NewString(),
		Version:    1,
		CreateTime: now,
		UpdateTime: now,
	}
	assignmentBytes, err := proto.Marshal(assignment)
	if err != nil {
		t.Fatalf("marshaling assignment: %v", err)
	}
	if _, err := pool.(*pgxpool.Pool).Exec(ctx, `INSERT INTO worker_assignments (actor_uid, worker_name, proto) VALUES ($1, $2, $3)`,
		assignment.GetActorUid(), workerName, assignmentBytes); err != nil {
		t.Fatalf("seeding assignment of actor %q on worker %q: %v", assignment.GetActorUid(), workerName, err)
	}
	worker, err := s.GetWorker(ctx, workerName)
	if err != nil {
		t.Fatalf("getting worker %q to book a seeded assignment: %v", workerName, err)
	}
	if _, err := s.UpdateWorker(ctx, workerName, store.PreconditionFrom(worker), func(w *ateapi.Worker) error {
		if w.Status == nil {
			w.Status = &ateapi.WorkerStatus{}
		}
		w.Status.Allocated = resources.AddToAllocated(w.Status.Allocated, assignment, +1)
		return nil
	}); err != nil {
		t.Fatalf("booking a seeded assignment on worker %q: %v", workerName, err)
	}
}

// WorkerHostingActor returns the name of the Worker holding an assignment for
// actorUID, or "" when none does.
func WorkerHostingActor(t *testing.T, ctx context.Context, s store.Interface, actorUID string) string {
	t.Helper()
	for workerToken := ""; ; {
		workers, err := s.ListWorkers(ctx, store.ListOptions{PageToken: workerToken})
		if err != nil {
			t.Fatalf("listing workers: %v", err)
		}
		for _, worker := range workers.Items {
			name := worker.GetMetadata().GetName()
			if _, err := s.GetWorkerAssignment(ctx, name, actorUID); err == nil {
				return name
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("getting the assignment of actor %q on worker %q: %v", actorUID, name, err)
			}
		}
		if !workers.HasNextPage() {
			return ""
		}
		workerToken = workers.NextPageToken
	}
}

// MustCreateTag ensures tag's parent atespace exists, then stores
// tag as given — ready or still pending, whichever state the test needs to
// start from. Use the store method directly only in tests that exercise
// missing-parent behavior.
func MustCreateTag(t *testing.T, ctx context.Context, s store.Interface, tag *ateapi.Tag) *ateapi.Tag {
	t.Helper()
	atespace := tag.GetMetadata().GetAtespace()
	name := tag.GetMetadata().GetName()
	MustCreateAtespace(t, ctx, s, atespace)
	created, err := s.CreateTag(ctx, tag)
	if err != nil {
		t.Fatalf("creating test tag %q/%q: %v", atespace, name, err)
	}
	return created
}

// RunTests runs m and terminates the shared PostgreSQL container afterwards.
// Packages with no other TestMain work should use it as their whole TestMain;
// the rest must call [Shutdown] themselves.
func RunTests(m *testing.M) {
	code := m.Run()
	Shutdown()
	os.Exit(code)
}

// Shutdown terminates the shared PostgreSQL container, if one was started.
func Shutdown() {
	freeDBMu.Lock()
	dbs := freeDBs
	freeDBs = nil
	freeDBMu.Unlock()
	for _, db := range dbs {
		db.fgaServer.Close()
		db.pool.Close()
	}
	if adminPool != nil {
		adminPool.Close()
		adminPool = nil
	}
	if containerPG != nil {
		if err := containerPG.Terminate(context.Background(), testcontainers.StopTimeout(0)); err != nil {
			fmt.Fprintf(os.Stderr, "terminating PostgreSQL testcontainer: %v\n", err)
		}
		containerPG = nil
	}
}

func requireAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	containerOnce.Do(func() {
		ctx := context.Background()
		if err := dockerenv.Configure(ctx); err != nil {
			containerErr = err
			return
		}
		containerPG, containerErr = postgres.Run(ctx, "postgres:18-alpine",
			postgres.WithDatabase("postgres"),
			postgres.WithUsername("postgres"),
			postgres.WithPassword("postgres"),
			testcontainers.WithCmdArgs(
				"-c", "synchronous_commit=off",
				"-c", "full_page_writes=off",
				"-c", "max_connections=500",
			),
			postgres.BasicWaitStrategies(),
		)
		if containerErr != nil {
			return
		}
		dsn, err := containerPG.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			containerErr = err
			Shutdown()
			return
		}
		adminCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			containerErr = err
			Shutdown()
			return
		}
		adminCfg.MaxConns = testPoolMaxConns
		adminCfg.MinConns = 0
		adminPool, containerErr = pgxpool.NewWithConfig(ctx, adminCfg)
		if containerErr != nil {
			Shutdown()
			return
		}
		var pingErr error
		for i := 0; i < 30; i++ {
			pingErr = adminPool.Ping(ctx)
			if pingErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if pingErr != nil {
			containerErr = fmt.Errorf("pinging PostgreSQL testcontainer after retries: %w", pingErr)
			Shutdown()
			return
		}
		if err := seedTemplateDatabase(ctx, adminPool); err != nil {
			containerErr = err
			Shutdown()
			return
		}
	})
	if containerErr != nil {
		if dockerenv.Required() {
			t.Fatalf("PostgreSQL testcontainer unavailable and required (CI or REQUIRE_DOCKER is set): %v", containerErr)
		}
		t.Skipf("PostgreSQL testcontainer unavailable (requires Docker): %v", containerErr)
	}
	return adminPool
}

// seedTemplateDatabase creates templateDatabaseName with Substrate migrations
// and the OpenFGA store and authorization model applied, then closes every
// connection to it so CREATE DATABASE ... TEMPLATE can copy it cleanly.
func seedTemplateDatabase(ctx context.Context, admin *pgxpool.Pool) error {
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDatabaseName); err != nil {
		return fmt.Errorf("creating PostgreSQL template database: %w", err)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = templateDatabaseName
	cfg.MaxConns = 2
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("opening PostgreSQL template database: %w", err)
	}
	defer pool.Close()

	persistence, err := atepg.NewPersistence(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrating PostgreSQL template database: %w", err)
	}
	defer persistence.Close()

	fgaServer, err := authz.NewOpenFGAServer(pool)
	if err != nil {
		return fmt.Errorf("creating OpenFGA server on template database: %w", err)
	}
	defer fgaServer.Close()

	if _, _, err := authz.EnsureStoreAndModel(ctx, pool, fgaServer); err != nil {
		return fmt.Errorf("seeding OpenFGA store and model on template database: %w", err)
	}
	return nil
}
