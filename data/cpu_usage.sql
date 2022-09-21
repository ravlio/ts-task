CREATE TABLE IF NOT EXISTS cpu_usage
(
    ts    TIMESTAMPTZ,
    host  TEXT,
    usage DOUBLE PRECISION
);
SELECT create_hypertable('cpu_usage', 'ts', if_not_exists=> true);
\COPY cpu_usage FROM /cpu_usage.csv CSV HEADER;