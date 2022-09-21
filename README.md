# Timescale Query Benchmark Tool

This tool is for Timescale database query performance testing. It uses **pgx** Postgres driver and prepared statements.

# Table of Contents
1. [How it works](#how-it-works)
2. [How to run with Docker](#how-to-run-with-docker)
3. [How to run from source](#how-to-run-from-source)
4. [Testing](#testing)
5. [Demo](#demo)
6. [Limitations](#limitations)

# How it works

Benchmark creates a pool of connections to the Postgres instance with the Timescale installed on it.

One or more workers are started.

Next, the tasks are loaded from the provided csv file.

The task entity is a set of **hostname**, **from date** and **to date**

Tasks are distributed among workers with the constraint that queries for the same hostname be executed by the same worker each time.

Each worker receives a task, creates a query based on the task payload, executes it and writes metrics.

Besides single counters, metrics include advanced statistics:
- median
- percentiles
- standard deviation

which is helpful for observations, but it requires additional in-memory allocations and adds limitations to query_params file length.

Example output:

```
199 tasks were loaded
Benchmarking...
queries processed: 199
query errors: 0
total processing time: 6.755646075s
min query time: 9.080253ms
max query time: 288.061007ms
avg query time: 33.94797ms
mean query time: 22.490951ms
10th percentile: 11.36448ms
50th percentile: 22.454277ms
90th percentile: 38.738246ms
standard deviation: 51.834798ms
```
# How to run with Docker

Docker image:

    docker.io/ravlio/ts-bench:0.1.0

Basic usage with local Postgres and Docker:
    
    docker run --network host docker.io/ravlio/ts-bench:0.1.0 --pg-dsn="postgres://postgres:postgres@localhost:5112/postgres"

Benchmark includes query_params.csv by default 

# How to run from source

Run with Golang:

    go run main.go

Usage:

- `csv-path` - query_params csv file path. Default: data/query_params.csv
- `pg-dsn` - postgresql dsn string. Default: postgres://postgres:postgres@localhost:5112/postgres
- `scan-rows` - additionally scan rows of results. Default: true
- `workers-count` - number of concurrent workers. Default: cpu count * 10 (`because of io-bound load`)
- `help` - this information

# Testing

To run integration tests:

    docker-compose run integration_test

# Demo
Run commands in sequence: 

    docker-compose run migrations
    docker-compose run bench

This will create a Timescale instance with a database, load migrations from data/cpu_usage.sql and run the benchmark with the default configuration.

# Limitations

Please take into account that series metrics require additional memory allocations for storing execution time for each query, so the total number of queries (query_params.csv) shouldn't be too big.