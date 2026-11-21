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
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"go.ate.dev/substrate/cmd/ateapi/internal/authz"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/dockerenv"
)

const (
	atepgTemplateDatabase = "atepg_template"
	atepgTestPoolMaxConns = 4
)

// One Postgres container serves every test in this package. Tests that call
// setupPostgresPersistence get an isolated database cloned from atepg_template.
var (
	containerOnce sync.Once
	containerPool *pgxpool.Pool
	containerDSN  string
	containerPG   *postgres.PostgresContainer
	containerErr  error
	createDBMu    sync.Mutex
	databaseCount atomic.Uint64
)

func TestMain(m *testing.M) {
	code := m.Run()
	if containerPool != nil {
		containerPool.Close()
	}
	if containerPG != nil {
		if err := containerPG.Terminate(context.Background(), testcontainers.StopTimeout(0)); err != nil {
			fmt.Fprintf(os.Stderr, "terminating PostgreSQL testcontainer: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	containerOnce.Do(func() {
		ctx := context.Background()
		if err := dockerenv.Configure(ctx); err != nil {
			containerErr = err
			return
		}
		pgContainer, err := postgres.Run(ctx, "postgres:18-alpine",
			postgres.WithDatabase("atepg"),
			postgres.WithUsername("atepg"),
			postgres.WithPassword("atepg"),
			testcontainers.WithCmdArgs(
				"-c", "synchronous_commit=off",
				"-c", "full_page_writes=off",
				"-c", "max_connections=500",
			),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			containerErr = err
			return
		}
		containerPG = pgContainer
		dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			containerErr = err
			return
		}
		containerDSN = dsn
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			containerErr = err
			return
		}
		cfg.MaxConns = atepgTestPoolMaxConns
		cfg.MinConns = 0
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			containerErr = err
			return
		}
		// The official postgres image restarts its server process once after
		// initdb; the port accepts (and briefly resets) connections during
		// that window, so ping with retries rather than failing on the first
		// attempt.
		var pingErr error
		for i := 0; i < 30; i++ {
			pingErr = pool.Ping(ctx)
			if pingErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if pingErr != nil {
			pool.Close()
			containerErr = fmt.Errorf("pinging PostgreSQL testcontainer after retries: %w", pingErr)
			return
		}
		if err := seedTemplateDatabase(ctx, pool); err != nil {
			pool.Close()
			containerErr = err
			return
		}
		containerPool = pool
	})
	if containerErr != nil {
		if dockerenv.Required() {
			t.Fatalf("PostgreSQL testcontainer unavailable and required (CI or REQUIRE_DOCKER is set): %v", containerErr)
		}
		t.Skipf("PostgreSQL testcontainer unavailable (requires Docker): %v", containerErr)
	}
	return containerPool
}

func seedTemplateDatabase(ctx context.Context, admin *pgxpool.Pool) error {
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+atepgTemplateDatabase); err != nil {
		return fmt.Errorf("creating PostgreSQL template database: %w", err)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = atepgTemplateDatabase
	cfg.MaxConns = 2
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("opening PostgreSQL template database: %w", err)
	}
	defer pool.Close()

	p, err := NewPersistence(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrating PostgreSQL template database: %w", err)
	}
	defer p.Close()

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

func cloneTemplatePool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()
	admin := requirePool(t)
	dbName := fmt.Sprintf("atepg_test_%d", databaseCount.Add(1))
	createDBMu.Lock()
	_, createErr := admin.Exec(ctx, "CREATE DATABASE "+dbName+" TEMPLATE "+atepgTemplateDatabase)
	createDBMu.Unlock()
	if createErr != nil {
		t.Fatalf("creating PostgreSQL test database: %v", createErr)
	}

	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = dbName
	cfg.MaxConns = atepgTestPoolMaxConns
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("opening PostgreSQL test database: %v", err)
	}
	return pool, func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+dbName); err != nil {
			t.Errorf("dropping PostgreSQL test database: %v", err)
		}
	}
}

func testConnectConfig(schema string) ConnectConfig {
	return ConnectConfig{
		ReadWriteDSN:  containerDSN,
		OwnerDSN:      containerDSN,
		ReadWriteRole: "atepg",
		OwnerRole:     "atepg",
		Schema:        schema,
	}
}
