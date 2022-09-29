package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/montanaflynn/stats"
)

// queryTimeBufferSize is a initial size for query times buffer
const queryTimeBufferSize = 10_000

// a metrics contains atomic counters and query time buffer
type metrics struct {
	queriesProcessed    uint64
	queryErrors         uint64
	totalProcessingTime time.Duration
	minQueryTime        time.Duration
	maxQueryTime        time.Duration
	queryTime           []float64
}

type queryResult struct {
	err            error
	processingTime time.Duration
}

// a task is a task payload for querying
type task struct {
	hostname string
	from     time.Time
	to       time.Time
}

// a bench is a benchmarking tool
type bench struct {
	metrics      metrics
	pool         *pgxpool.Pool
	csvReader    *csv.Reader
	workersCount int
	scanRows     bool
	// buffered task buffer for each worker
	// tasks are mapped to workers by hostname hash
	tasksCh   []chan task
	metricsCh chan queryResult
	tasksWg   sync.WaitGroup
}

type options struct {
	pool         *pgxpool.Pool
	csvReader    io.Reader
	workersCount int
	scanRows     bool
}

// newBench creates new bench tool
func newBench(opts *options) *bench {
	m := metrics{
		queriesProcessed:    0,
		queryErrors:         0,
		totalProcessingTime: 0,
		minQueryTime:        time.Duration(math.MaxInt64),
		maxQueryTime:        0,
		// queryTime handles a series of query processing times to be able to calculate median and percentiles
		queryTime: make([]float64, 0, queryTimeBufferSize),
	}
	b := &bench{
		metrics:      m,
		pool:         opts.pool,
		scanRows:     opts.scanRows,
		csvReader:    csv.NewReader(opts.csvReader),
		workersCount: opts.workersCount,
		tasksCh:      make([]chan task, opts.workersCount),
		metricsCh:    make(chan queryResult, opts.workersCount),
	}

	// initialize buffered task channel for each worker
	for i := 0; i < opts.workersCount; i++ {
		b.tasksCh[i] = make(chan task, 100)
	}
	b.tasksWg.Add(opts.workersCount)

	return b
}

// a run runs benchmark and waits completion. Can be cancelled
func (b *bench) run(ctx context.Context) (err error) {
	fmt.Println("Reading tasks...")
	tasks, err := b.readTasks(ctx)
	if err != nil {
		return err
	}

	go b.runMetrics(ctx)

	fmt.Printf("%d tasks were loaded\n", len(tasks))
	go func() {
		for _, t := range tasks {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Send task to a worker.
			// Queries for the same hostname are assigned to the same worker.
			// We use crc32 hash of hostname to convert it into int.
			h := fnv.New32a()
			h.Write([]byte(t.hostname))
			// We calculate worker/bucked by using modulo
			bucket := h.Sum32() % uint32(b.workersCount)
			b.tasksCh[bucket] <- t
		}

		for _, ch := range b.tasksCh {
			close(ch)
		}
	}()
	fmt.Println("Benchmarking...")

	for i := 0; i < b.workersCount; i++ {
		// run worker with dedicated task channel
		go b.runTaskProcessor(ctx, b.tasksCh[i])
	}

	b.tasksWg.Wait()
	close(b.metricsCh)

	return
}

// runMetrics runs metric collector which collects metrics from workers
func (b *bench) runMetrics(ctx context.Context) {
	for m := range b.metricsCh {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if m.err != nil {
			b.metrics.queryErrors++
			continue
		}

		b.metrics.queriesProcessed++
		b.metrics.totalProcessingTime++

		// store all the rest metrics
		b.metrics.totalProcessingTime += m.processingTime
		if m.processingTime < b.metrics.minQueryTime {
			b.metrics.minQueryTime = m.processingTime
		}
		if m.processingTime >= b.metrics.maxQueryTime {
			b.metrics.maxQueryTime = m.processingTime
		}

		// We store each query time as an individual value in slice
		// so that we can apply statistic functions like median and percentiles
		// it may be memory inefficient
		b.metrics.queryTime = append(b.metrics.queryTime, float64(m.processingTime))
	}
}

// readTasks reads csv file and sends tasks hashed by hostname to workers
func (b *bench) readTasks(ctx context.Context) ([]task, error) {
	var ret = make([]task, 0, 10*1024)
	// skip header
	if _, err := b.csvReader.Read(); err != nil {
		return nil, fmt.Errorf("can't read csv header: %w", err)
	}

	var line = 0
Loop:
	for {
		select {
		case <-ctx.Done():
			break Loop
		default:
		}

		line++
		record, err := b.csvReader.Read()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("can't read csv as line %d: %w", line, err)
		}

		hostname := record[0]
		from, err := time.Parse("2006-01-02 15:04:05", record[1])
		if err != nil {
			return nil, fmt.Errorf("can't parse From value %q as line %d: %w", record[1], line, err)
		}

		to, err := time.Parse("2006-01-02 15:04:05", record[2])
		if err != nil {
			return nil, fmt.Errorf("can't parse To value %q: %w", record[1], err)
		}

		t := task{
			hostname: hostname,
			from:     from,
			to:       to,
		}

		ret = append(ret, t)
	}

	return ret, nil
}

// runTaskProcessor runs worker that reads tasks from ch, executes queries and writes statistics
func (b *bench) runTaskProcessor(ctx context.Context, ch chan task) {
	var err error

Loop:
	for {
		select {
		case <-ctx.Done():
			break Loop
		case t, ok := <-ch:
			if !ok {
				break Loop
			}
			start := time.Now()

			// run query
			err = b.query(ctx, t.hostname, t.from, t.to)
			b.metricsCh <- queryResult{processingTime: time.Since(start), err: err}
		}
	}

	b.tasksWg.Done()
}

// query build query with provided arguments and executes it
func (b *bench) query(ctx context.Context, hostname string, from, to time.Time) error {
	const query = `
	SELECT time_bucket_gapfill('1 minute', ts) AS minute,
		min(usage)                          AS min_usage,
		max(usage)                          AS max_usage
	FROM cpu_usage
	where host = $1
	and ts between $2 and $3
	GROUP BY minute`

	// execute query
	rows, err := b.pool.Query(
		ctx,
		query,
		hostname,
		from,
		to,
	)
	if err != nil {
		return fmt.Errorf("can't execute query: %w", err)
	}

	// optionally scan the results so we can benchmark this part as well
	if b.scanRows {
		var min, max float64
		var ts time.Time

		for rows.Next() {
			err = rows.Scan(&min, &max, &ts)
			if err != nil {
				return fmt.Errorf("can't read row: %w", err)
			}
		}
	}
	rows.Close()

	return nil
}

// displayMetrics write all the metrics to w writer
func (b *bench) displayMetrics(w io.Writer) error {
	sb := strings.Builder{}

	// single counter metrics
	sb.Write([]byte(fmt.Sprintf("queries processed: %d\n", b.metrics.queriesProcessed)))

	sb.Write([]byte(fmt.Sprintf("query errors: %d\n", b.metrics.queryErrors)))

	sb.Write([]byte(fmt.Sprintf("total processing time: %s\n", b.metrics.totalProcessingTime)))

	if b.metrics.queriesProcessed == 0 {
		_, err := w.Write([]byte(sb.String()))

		return err
	}

	sb.Write([]byte(fmt.Sprintf("min query time: %s\n", b.metrics.minQueryTime)))

	sb.Write([]byte(fmt.Sprintf("max query time: %s\n", b.metrics.maxQueryTime)))

	avgTime := b.metrics.totalProcessingTime / time.Duration(b.metrics.queriesProcessed)
	sb.Write([]byte(fmt.Sprintf("avg query time: %s\n", avgTime)))

	// slice metrics
	median, err := stats.Median(b.metrics.queryTime)
	if err == nil {
		sb.Write([]byte(fmt.Sprintf("median query time: %s\n", displayDuration(uint64(median)))))
	}

	p10, err := stats.Percentile(b.metrics.queryTime, 10)
	if err == nil {
		sb.Write([]byte(fmt.Sprintf("10th percentile: %s\n", displayDuration(uint64(p10)))))
	}

	p50, err := stats.Percentile(b.metrics.queryTime, 50)
	if err == nil {
		sb.Write([]byte(fmt.Sprintf("50th percentile: %s\n", displayDuration(uint64(p50)))))
	}

	p90, err := stats.Percentile(b.metrics.queryTime, 90)
	if err == nil {
		sb.Write([]byte(fmt.Sprintf("90th percentile: %s\n", displayDuration(uint64(p90)))))
	}

	stddev, err := stats.StandardDeviation(b.metrics.queryTime)
	if err == nil {
		sb.Write([]byte(fmt.Sprintf("standard deviation: %s\n", displayDuration(uint64(stddev)))))
	}

	_, err = w.Write([]byte(sb.String()))

	return err
}

// displayDuration is a helper that converts uint64 to duration and displays it
func displayDuration(di uint64) string {
	return fmt.Sprintf("%s", time.Duration(di))
}

func run() error {
	var dsn = flag.String("pg-dsn", "postgres://postgres:postgres@localhost:5112/postgres", "postgresql dsn string")
	var csvPath = flag.String("csv-path", "data/query_params.csv", "query_params csv file path")
	var workersCount = flag.Int("workers-count", runtime.NumCPU()*10, "number of concurrent workers")
	var scanRows = flag.Bool("scan-rows", true, "additionally scan rows of results")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage:\n")

		flag.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(os.Stderr, "    %s - %s. Default: %s\n", f.Name, f.Usage, f.DefValue)
		})
	}
	flag.Parse()

	pool, err := pgxpool.Connect(context.Background(), *dsn)
	if err != nil {
		return fmt.Errorf("unable to connect to database: %w", err)
	}
	defer pool.Close()

	csvRdr, err := os.Open(*csvPath)
	if err != nil {
		return fmt.Errorf("unable to connect to database: %w", err)
	}
	defer csvRdr.Close()

	opts := &options{
		pool:         pool,
		csvReader:    csvRdr,
		workersCount: *workersCount,
		scanRows:     *scanRows,
	}
	b := newBench(opts)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-ctx.Done():
		case s := <-sig:
			cancel()
			fmt.Printf("`%s` signal received", s.String())
		}
	}()

	err = b.run(ctx)
	if err != nil {
		return err
	}

	err = b.displayMetrics(os.Stdout)
	if err != nil {
		return err
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "benchmark error: %s\n", err.Error())
		os.Exit(1)
	}
}
