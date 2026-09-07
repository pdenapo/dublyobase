package core

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A role holding only SELECT still has to see primary and foreign keys.
// information_schema.table_constraints hides constraints from such a role — it
// only lists tables the role owns or holds a privilege other than SELECT on —
// so discovery reported every table as "table has no primary key" and refused
// to import any of them. The catalog queries must not regress to that view.
func TestConstraintDiscoveryWithSelectOnlyRole(t *testing.T) {
	owner := testPool(t)
	ctx := context.Background()

	const setup = `
		drop schema if exists discovery_probe cascade;
		do $$ begin
			if exists (select 1 from pg_roles where rolname = 'discovery_reader') then
				execute 'drop owned by discovery_reader';
				execute 'drop role discovery_reader';
			end if;
		end $$;
		create schema discovery_probe;
		create table discovery_probe.parent (id text primary key);
		create table discovery_probe.child (
			id uuid primary key,
			parent_id text not null references discovery_probe.parent(id) on delete cascade
		);
		create role discovery_reader login password 'probe-only';
		grant usage on schema discovery_probe to discovery_reader;
		grant select on all tables in schema discovery_probe to discovery_reader;`
	if _, err := owner.Exec(ctx, setup); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(ctx, `
			drop schema if exists discovery_probe cascade;
			do $$ begin
				if exists (select 1 from pg_roles where rolname = 'discovery_reader') then
					execute 'drop owned by discovery_reader';
					execute 'drop role discovery_reader';
				end if;
			end $$;`)
	})

	dsn := os.Getenv("TEST_DATABASE_URL")
	readerDSN, err := replaceDSNCredentials(dsn, "discovery_reader", "probe-only")
	if err != nil {
		t.Fatalf("reader dsn: %v", err)
	}
	reader, err := pgxpool.New(ctx, readerDSN)
	if err != nil {
		t.Fatalf("reader pool: %v", err)
	}
	defer reader.Close()

	// Guard the premise: if the SQL-standard view stopped hiding constraints
	// this test would pass for the wrong reason.
	var viaInformationSchema int
	if err := reader.QueryRow(ctx, `
		select count(*) from information_schema.table_constraints
		where table_schema = 'discovery_probe' and constraint_type = 'PRIMARY KEY'`).Scan(&viaInformationSchema); err != nil {
		t.Fatalf("information_schema probe: %v", err)
	}
	if viaInformationSchema != 0 {
		t.Skipf("information_schema exposed %d constraints to a select-only role; premise no longer holds", viaInformationSchema)
	}

	pk, err := discoverPrimaryKey(ctx, reader, "discovery_probe", "child")
	if err != nil {
		t.Fatalf("discoverPrimaryKey: %v", err)
	}
	if len(pk) != 1 || pk[0] != "id" {
		t.Fatalf("primary key = %v, want [id]", pk)
	}

	fks, err := discoverForeignKeys(ctx, reader, "discovery_probe", "child")
	if err != nil {
		t.Fatalf("discoverForeignKeys: %v", err)
	}
	if len(fks) != 1 {
		t.Fatalf("foreign keys = %v, want 1", fks)
	}
	got := fks[0]
	if got.Column != "parent_id" || got.TargetSchema != "discovery_probe" || got.TargetTable != "parent" || got.TargetColumn != "id" {
		t.Fatalf("foreign key = %+v, want discovery_probe.parent(id) from parent_id", got)
	}
	if got.OnDelete != "CASCADE" {
		t.Fatalf("on delete = %q, want CASCADE", got.OnDelete)
	}
}

// replaceDSNCredentials swaps the userinfo of a postgres URL so the test can
// reconnect as an unprivileged role without a second environment variable.
func replaceDSNCredentials(dsn, user, password string) (string, error) {
	const scheme = "://"
	i := strings.Index(dsn, scheme)
	if i < 0 {
		return "", errInvalidTestDSN
	}
	rest := dsn[i+len(scheme):]
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return dsn[:i+len(scheme)] + user + ":" + password + "@" + rest, nil
}
