// Package db owns the Postgres connection and the jobs schema.
package db

import (
	"context"
	_ "embed"
	"fmt"
	neturl "net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Connect opens a pool and checks the database is reachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// migrateLockID names the advisory lock that serializes migrations.
const migrateLockID = 7_316_002 // arbitrary, unique to Conduit

// Migrate applies the schema. It is idempotent, so it runs on every startup.
// The API and workers often start together, and CREATE ... IF NOT EXISTS is
// not safe to run concurrently (both see no table, both create it, one fails),
// so the schema runs in one transaction holding an advisory lock: the others
// wait, then find everything already there.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Sibling returns the URL of database name on the same server as url,
// creating the database if it does not exist.
func Sibling(ctx context.Context, url, name string) (string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			return "", fmt.Errorf("create database %s: %w", name, err)
		}
	}

	u, err := neturl.Parse(url)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}
