package main

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestBench(t *testing.T) {
	// integration tests
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// the real connection is used because mocking postgres protocol is a tricky and requires more time
	pool, err := pgxpool.Connect(ctx, os.Getenv("PG_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	require.NoError(t, err)
	m := `CREATE TABLE IF NOT EXISTS cpu_usage
(
    ts    TIMESTAMPTZ,
    host  TEXT,
    usage DOUBLE PRECISION
);
SELECT create_hypertable('cpu_usage', 'ts', if_not_exists=> true);`
	_, err = pool.Exec(ctx, m)
	require.NoError(t, err)

	csv := `hostname,start_time,end_time
host_000008,2017-01-01 08:59:22,2017-01-01 09:59:22
host_000008,2017-01-01 08:59:22,2017-01-01 09:59:22
`
	opts := &options{
		pool:         pool,
		csvReader:    bytes.NewReader([]byte(csv)),
		workersCount: 5,
		scanRows:     false,
	}
	bench := newBench(opts)
	require.NoError(t, bench.run(context.Background()))
	err = bench.displayMetrics(ioutil.Discard)
	require.NoError(t, err)
	require.Equal(t, uint64(2), bench.metrics.queriesProcessed)
	require.Zero(t, bench.metrics.queryErrors)
}
