package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	errLog := log.New(os.Stderr, "[ERROR]: ", log.LstdFlags)
	infoLog := log.New(os.Stdout, "[INFO]: ", log.LstdFlags)
	var (
		listenAddr                               string
		interval                                 time.Duration
		bucketsStart, bucketsWidth, bucketsCount int
	)

	flag.StringVar(&listenAddr, "addr", ":8080", "Address to serve metrics on")
	flag.DurationVar(&interval, "interval", 30*time.Second, "Interval to run speed test at (ideally this matches the Prometheus scrape_interval)")
	flag.IntVar(&bucketsStart, "start", 5, "Value for the lowest bucket in the distribution")
	flag.IntVar(&bucketsWidth, "width", 5, "Width for each bucket in the distribution")
	flag.IntVar(&bucketsCount, "count", 60, "Count of buckets in the distribution")
	flag.Parse()
	if secs := interval.Seconds(); secs < 15 {
		errLog.Fatalf("Interval (%ds) must be >= 15s", interval.Seconds())
	}
	path, err := exec.LookPath("fast-cli")
	if err != nil {
		errLog.Fatalf("Unable to find fast-cli in path")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errorCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "speedtest_errors",
		Help: "Counter of errors occurred while running a speedtest",
	}, []string{"reason"})
	mbpsDistribution := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "speedtest_mbps",
		Help:    "Speedtest measurements in Mbps",
		Buckets: prometheus.LinearBuckets(float64(bucketsStart), float64(bucketsWidth), bucketsCount),
	})
	mbpsGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "speedtest_mbps_gauge",
		Help: "Speedtest measurements in Mbps",
	})
	latencyDistribution := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "speedtest_latency_secs",
		Help:    "Speedtest latency in TODO",
		Buckets: prometheus.LinearBuckets(1, 1, 30),
	})
	prometheus.MustRegister(errorCounter, mbpsDistribution, mbpsGauge, latencyDistribution)
	s := http.Server{
		Addr:    listenAddr,
		Handler: promhttp.Handler(),
	}
	go func() {
		if err := s.ListenAndServe(); err != nil {
			errLog.Printf("ListenAndServe returned unexpected error: %s", err)
		}
		cancel()
	}()

	runTest := func() {
		ctx, cancel := context.WithTimeout(ctx, interval-100*time.Millisecond)
		defer cancel()

		defer func(start time.Time) {
			elapsed := time.Now().Sub(start).Seconds()
			latencyDistribution.Observe(elapsed)
		}(time.Now())

		b, err := exec.CommandContext(ctx, path, "--simple").Output()
		if err != nil {
			errorCounter.With(prometheus.Labels{"reason": "command"}).Inc()
			errLog.Printf("Unable to execute command: %s", err)
			return
		}
		mbps, err := parseOutput(string(b))
		if err != nil {
			errorCounter.With(prometheus.Labels{"reason": "unexpected_output"}).Inc()
			errLog.Printf("Unable to parse output: %s", err)
			return
		}

		mbpsDistribution.Observe(mbps)
		mbpsGauge.Set(mbps)
		infoLog.Printf("Recorded measurement: %.2f Mbps", mbps)
	}
	runTest()

	// For now, simply run the speedtest periodically. It may be "cleaner" to
	// implement the prometheus.Collector interface and only run a test when
	// the scraper runs, but for now... this works.
	t := time.NewTicker(interval)
	for {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); !errors.Is(err, context.Canceled) {
				errLog.Fatalf("Terminating with unexpected error: %s", err)
			}
			infoLog.Printf("Terminating with expected signal")
			os.Exit(0)
		case <-t.C:
			runTest()
		}
	}
}

func parseOutput(s string) (float64, error) {
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, fmt.Errorf("expected 2 fields, got %d", len(fields))
	}
	if fields[1] != "Mbps" {
		return 0, fmt.Errorf("expected unit Mbps, got %q", fields[1])
	}
	mbps, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("value is not float64: %w", err)
	}
	return mbps, nil
}
