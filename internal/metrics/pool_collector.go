package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PoolStat is a provider-agnostic snapshot of pgxpool.Stat. Decouples the
// metrics package from jackc/pgx so tests can inject arbitrary values.
type PoolStat struct {
	Total            int32
	Acquired         int32
	Idle             int32
	Max              int32
	Constructing     int32
	AcquireCount     int64
	AcquireDuration  time.Duration
	NewConns         int64
	LifetimeDestroys int64
	IdleDestroys     int64
	EmptyAcquires    int64
	CanceledAcquires int64
	EmptyAcquireWait time.Duration
}

// PoolStatProvider yields a PoolStat snapshot on demand. Implementations must
// tolerate concurrent Stat() calls from the Prometheus scrape goroutine.
type PoolStatProvider interface {
	Stat() PoolStat
}

type poolCollector struct {
	provider PoolStatProvider

	totalConns        *prometheus.Desc
	acquiredConns     *prometheus.Desc
	idleConns         *prometheus.Desc
	maxConns          *prometheus.Desc
	constructingConns *prometheus.Desc
	acquireCount      *prometheus.Desc
	acquireDuration   *prometheus.Desc
	newConns          *prometheus.Desc
	lifetimeDestroys  *prometheus.Desc
	idleDestroys      *prometheus.Desc
	emptyAcquires     *prometheus.Desc
	canceledAcquires  *prometheus.Desc
	emptyAcquireWait  *prometheus.Desc
}

func newPoolCollector(provider PoolStatProvider) *poolCollector {
	return &poolCollector{
		provider: provider,
		totalConns: prometheus.NewDesc(
			"aiproxy_db_pool_total_connections",
			"Current total connections in the pgx pool (idle + acquired + constructing).",
			nil, nil,
		),
		acquiredConns: prometheus.NewDesc(
			"aiproxy_db_pool_acquired_connections",
			"Connections currently checked out of the pgx pool.",
			nil, nil,
		),
		idleConns: prometheus.NewDesc(
			"aiproxy_db_pool_idle_connections",
			"Idle connections available in the pgx pool.",
			nil, nil,
		),
		maxConns: prometheus.NewDesc(
			"aiproxy_db_pool_max_connections",
			"Maximum connections the pgx pool is configured to open.",
			nil, nil,
		),
		constructingConns: prometheus.NewDesc(
			"aiproxy_db_pool_constructing_connections",
			"Connections the pgx pool is currently constructing.",
			nil, nil,
		),
		acquireCount: prometheus.NewDesc(
			"aiproxy_db_pool_acquire_count_total",
			"Cumulative successful acquires from the pgx pool.",
			nil, nil,
		),
		acquireDuration: prometheus.NewDesc(
			"aiproxy_db_pool_acquire_duration_seconds_total",
			"Cumulative time spent waiting to acquire a connection, in seconds.",
			nil, nil,
		),
		newConns: prometheus.NewDesc(
			"aiproxy_db_pool_new_connections_total",
			"Cumulative new connections opened by the pgx pool.",
			nil, nil,
		),
		lifetimeDestroys: prometheus.NewDesc(
			"aiproxy_db_pool_lifetime_destroys_total",
			"Cumulative connections destroyed after hitting max lifetime.",
			nil, nil,
		),
		idleDestroys: prometheus.NewDesc(
			"aiproxy_db_pool_idle_destroys_total",
			"Cumulative connections destroyed after hitting max idle.",
			nil, nil,
		),
		emptyAcquires: prometheus.NewDesc(
			"aiproxy_db_pool_empty_acquires_total",
			"Cumulative acquires that had to wait for an available connection.",
			nil, nil,
		),
		canceledAcquires: prometheus.NewDesc(
			"aiproxy_db_pool_canceled_acquires_total",
			"Cumulative acquires canceled via context before a connection was available.",
			nil, nil,
		),
		emptyAcquireWait: prometheus.NewDesc(
			"aiproxy_db_pool_empty_acquire_wait_seconds_total",
			"Cumulative time spent waiting when no connection was available, in seconds.",
			nil, nil,
		),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalConns
	ch <- c.acquiredConns
	ch <- c.idleConns
	ch <- c.maxConns
	ch <- c.constructingConns
	ch <- c.acquireCount
	ch <- c.acquireDuration
	ch <- c.newConns
	ch <- c.lifetimeDestroys
	ch <- c.idleDestroys
	ch <- c.emptyAcquires
	ch <- c.canceledAcquires
	ch <- c.emptyAcquireWait
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	if c.provider == nil {
		return
	}
	s := c.provider.Stat()
	ch <- prometheus.MustNewConstMetric(c.totalConns, prometheus.GaugeValue, float64(s.Total))
	ch <- prometheus.MustNewConstMetric(c.acquiredConns, prometheus.GaugeValue, float64(s.Acquired))
	ch <- prometheus.MustNewConstMetric(c.idleConns, prometheus.GaugeValue, float64(s.Idle))
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(s.Max))
	ch <- prometheus.MustNewConstMetric(c.constructingConns, prometheus.GaugeValue, float64(s.Constructing))
	ch <- prometheus.MustNewConstMetric(c.acquireCount, prometheus.CounterValue, float64(s.AcquireCount))
	ch <- prometheus.MustNewConstMetric(c.acquireDuration, prometheus.CounterValue, s.AcquireDuration.Seconds())
	ch <- prometheus.MustNewConstMetric(c.newConns, prometheus.CounterValue, float64(s.NewConns))
	ch <- prometheus.MustNewConstMetric(c.lifetimeDestroys, prometheus.CounterValue, float64(s.LifetimeDestroys))
	ch <- prometheus.MustNewConstMetric(c.idleDestroys, prometheus.CounterValue, float64(s.IdleDestroys))
	ch <- prometheus.MustNewConstMetric(c.emptyAcquires, prometheus.CounterValue, float64(s.EmptyAcquires))
	ch <- prometheus.MustNewConstMetric(c.canceledAcquires, prometheus.CounterValue, float64(s.CanceledAcquires))
	ch <- prometheus.MustNewConstMetric(c.emptyAcquireWait, prometheus.CounterValue, s.EmptyAcquireWait.Seconds())
}
