// Copyright 2020 Google Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package export

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/compute/metadata"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/tsdb/record"
	"google.golang.org/api/option"
	monitoring_pb "google.golang.org/genproto/googleapis/monitoring/v3"
	"google.golang.org/grpc"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	samplesExported = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gcm_collector_samples_exported_total",
			Help: "Number of samples exported at scrape time.",
		},
	)
	samplesDropped = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gcm_collector_samples_dropped_total",
			Help: "Number of exported samples that were dropped because shard queues were full.",
		},
	)
	samplesSent = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gcm_collector_samples_sent_total",
			Help: "Number of exported samples sent to GCM.",
		},
	)
	sendIterations = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gcm_collector_send_iterations_total",
			Help: "Number of processing iterations of the sample export send handler.",
		},
	)
	shardProcess = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gcm_collector_shard_process_total",
			Help: "Number of shard retrievals.",
		},
	)
	shardProcessPending = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gcm_collector_shard_process_pending_total",
			Help: "Number of shard retrievals with an empty result.",
		},
	)
)

// Exporter converts Prometheus samples into Cloud Monitoring samples and exporst them.
type Exporter struct {
	logger log.Logger
	opts   ExporterOpts

	seriesCache *seriesCache
	builder     *sampleBuilder
	shards      []shard

	// Channel for signaling that there may be more work items to
	// be processed.
	nextc chan struct{}
}

const (
	// Number of shards by which series are bucketed.
	shardCount = 512
	// Buffer size for each individual shard.
	shardBufferSize = 2048

	// Maximum number of samples to pack into a batch sent to GCM.
	batchSizeMax = 200
	// Time after an accumulating batch is flushed to GCM. This avoids data being
	// held indefinititely if not enough new data flows in to fill up the batch.
	// Keeping it at just 5 seconds generally prevents two scrapes of the same target
	// making it into the same batch, which would trigger an error in GCM.
	// This saves us implementing detection logic for a case only affecting tiny servers.
	batchDelayMax = 5 * time.Second
)

// ExporterOpts holds options for an exporter.
type ExporterOpts struct {
	// Google Cloud project ID to which data is sent.
	ProjectID string
	// Test endpoint to send data to instead of GCM API
	TestEndpoint string
	// Credentials file for authentication with the GCM API.
	CredentialsFile string
}

// NewFlagOptions returns new exporter options that are populated through flags
// registered in the given application.
func NewFlagOptions(a *kingpin.Application) *ExporterOpts {
	var opts ExporterOpts

	// Default to the project ID if we can detect it.
	var projectID string
	if metadata.OnGCE() {
		projectID, _ = metadata.ProjectID()
	}

	a.Flag("gcm.experimental.project_id", "Google Cloud project ID to which data is sent.").
		Default(projectID).StringVar(&opts.ProjectID)

	a.Flag("gcm.experimental.test_endpoint", "Test endpoint to send data to instead of GCM API.").
		StringVar(&opts.TestEndpoint)

	a.Flag("gcm.experimental.credentials_file", "Credentials file for authentication with the GCM API.").
		StringVar(&opts.CredentialsFile)

	return &opts
}

// New returns a new Cloud Monitoring Exporter.
func New(logger log.Logger, reg prometheus.Registerer, opts ExporterOpts) (*Exporter, error) {
	grpc_prometheus.EnableClientHandlingTimeHistogram()

	if logger == nil {
		logger = log.NewNopLogger()
	}
	if reg != nil {
		reg.MustRegister(
			samplesExported,
			samplesDropped,
			samplesSent,
			sendIterations,
			shardProcess,
			shardProcessPending,
		)
	}
	seriesCache := newSeriesCache(logger, metricsPrefix)

	if opts.ProjectID == "" {
		return nil, errors.New("GCP project ID missing")
	}

	e := &Exporter{
		logger:      logger,
		opts:        opts,
		nextc:       make(chan struct{}, 1),
		seriesCache: seriesCache,
		builder:     &sampleBuilder{series: seriesCache},
		shards:      make([]shard, shardCount),
	}
	for i := range e.shards {
		e.shards[i] = newShard(shardBufferSize)
	}

	return e, nil
}

// Generally, global state is not a good approach and actively discouraged throughout
// the Prometheus code bases. However, this is the most practical way to inject the export
// path into lower layers of Prometheus without touching an excessive amount of functions
// in our fork to propagate it.
var globalExporter *Exporter

// InitGlobal initializes the global instance of the GCM exporter.
func InitGlobal(logger log.Logger, reg prometheus.Registerer, opts ExporterOpts) (err error) {
	globalExporter, err = New(logger, reg, opts)
	return err
}

// Global returns the global instance of the GCM exporter.
func Global() *Exporter {
	if globalExporter == nil {
		panic("Global GCM exporter used before initialization.")
	}
	return globalExporter
}

// SetLabelsByIDFunc injects a function that can be used to retrieve a label set
// based on a series ID we got through exported sample records.
// Must be called before any call to Export is made.
func (e *Exporter) SetLabelsByIDFunc(f func(uint64) labels.Labels) {
	e.seriesCache.getLabelsByRef = f
}

// Export enqueues the samples to be written to Cloud Monitoring.
func (e *Exporter) Export(target *scrape.Target, samples []record.RefSample) {
	var (
		sample *monitoring_pb.TimeSeries
		hash   uint64
		err    error
	)
	for len(samples) > 0 {
		sample, hash, samples, err = e.builder.next(target, samples)
		if err != nil {
			panic(err)
		}
		if sample != nil {
			// TODO(freinartz): decouple sending from ingestion by writing to a
			// dedicated write-ahead-log here from which the send queues consume.
			e.enqueue(hash, sample)
		}
	}
	// Signal that new data is available.
	e.triggerNext()
}

func (e *Exporter) enqueue(hash uint64, sample *monitoring_pb.TimeSeries) {
	idx := hash % uint64(len(e.shards))
	e.shards[idx].enqueue(hash, sample)
}

func (e *Exporter) triggerNext() {
	select {
	case e.nextc <- struct{}{}:
	default:
	}
}

// Run sends exported samples to Google Cloud Monitoring.
func (e *Exporter) Run(ctx context.Context) error {
	clientOpts := []option.ClientOption{
		option.WithGRPCDialOption(grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor)),
	}
	if e.opts.TestEndpoint != "" {
		clientOpts = append(clientOpts,
			option.WithEndpoint(e.opts.TestEndpoint),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithInsecure()),
		)
	}
	if e.opts.CredentialsFile != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(e.opts.CredentialsFile))
	}
	metricClient, err := monitoring.NewMetricClient(ctx, clientOpts...)
	if err != nil {
		return err
	}
	defer metricClient.Close()

	go e.seriesCache.run(ctx)

	timer := time.NewTimer(batchDelayMax)
	stopTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stopTimer()

	var (
		batch = make([]*monitoring_pb.TimeSeries, 0, batchSizeMax)
		// Cache of series hashes already seen in the current batch.
		seen = make(map[uint64]struct{}, batchSizeMax)
		// Functions to be called once the batch has been sent.
		closers = make([]func(), 0, shardCount)
	)

	// Send the currently accumulated batch to GCM asynchronously.
	send := func() {
		go func(batch []*monitoring_pb.TimeSeries, closers []func()) {
			if err := e.send(ctx, metricClient, batch); err != nil {
				level.Error(e.logger).Log("msg", "send batch", "err", err)
			}
			samplesSent.Add(float64(len(batch)))

			for _, close := range closers {
				close()
			}
		}(batch, closers)

		// Reset state for new batch.
		stopTimer()
		timer.Reset(batchDelayMax)

		for k := range seen {
			delete(seen, k)
		}

		closers = make([]func(), 0, shardCount)
		batch = make([]*monitoring_pb.TimeSeries, 0, batchSizeMax)
	}

	// Starting index when iterating over shards.
	shardOffset := 0

	for {
		select {
		// NOTE(freinartz): we will terminate once context is cancelled and not flush remaining
		// buffered data. In-flight requests will be aborted as well.
		// This is fine once we persist data submitted via Export() but for now there may be some
		// data loss on shutdown.
		case <-ctx.Done():
			return nil
		// This is activated for each new sample that arrives
		case <-e.nextc:
			sendIterations.Inc()

			// Drain shards to fill up the batch.
			//
			// If the shard count is high given the overall throughput, a lot of shards may
			// be packed into the same batch. A slow request will then block all those shards
			// from further parallel sends.
			// If this becomes a problem (especially when we grow maximum batch size), consider
			// adding a heuristic to send partial batches in favor of limiting the number of
			// shards they span.
			i := 0
			for ; i < len(e.shards); i++ {
				shardProcess.Inc()
				index := (i + shardOffset) % len(e.shards)
				shard := e.shards[index]

				if shard.pending {
					shardProcessPending.Inc()
					continue
				}
				// Populate the batch until it's full or the shard is empty.
				startLen := len(batch)
				for len(batch) < cap(batch) {
					e, ok := shard.get()
					if !ok {
						break
					}
					// If a series is about to be added that's already in the batch, flush
					// it and start a new one.
					if _, ok := seen[e.hash]; ok {
						send()
					}
					seen[e.hash] = struct{}{}
					batch = append(batch, e.sample)
				}
				if len(batch) > startLen {
					shard.pending = true
					closers = append(closers, func() { shard.pending = false })
				}
				if len(batch) == cap(batch) {
					send()
				}
			}
			// If we didn't make a full pass over all shards, there may be more work.
			if i < len(e.shards) {
				e.triggerNext()
			}
			shardOffset = (shardOffset + i) % len(e.shards)

		case <-timer.C:
			// Flush batch that has been pending for too long.
			if len(batch) > 0 {
				send()
			} else {
				timer.Reset(batchDelayMax)
			}
		}
	}
}

func (e *Exporter) send(ctx context.Context, client *monitoring.MetricClient, batch []*monitoring_pb.TimeSeries) error {
	// TODO(freinartz): Handle retries if the error type allows.
	return client.CreateTimeSeries(ctx, &monitoring_pb.CreateTimeSeriesRequest{
		Name:       fmt.Sprintf("projects/%s", e.opts.ProjectID),
		TimeSeries: batch,
	})
}

// shard holds a queue of data for a subset of samples.
type shard struct {
	queue   chan queueEntry
	pending bool
}

type queueEntry struct {
	hash   uint64
	sample *monitoring_pb.TimeSeries
}

func newShard(queueSize int) shard {
	return shard{queue: make(chan queueEntry, queueSize)}
}

// get oldest queue entry if it exists.
func (s *shard) get() (queueEntry, bool) {
	select {
	case e, ok := <-s.queue:
		return e, ok
	default:
	}
	return queueEntry{}, false
}

func (s *shard) enqueue(hash uint64, sample *monitoring_pb.TimeSeries) {
	samplesExported.Inc()

	e := queueEntry{
		hash:   hash,
		sample: sample,
	}
	select {
	case s.queue <- e:
	default:
		// TODO(freinartz): tail drop is not a great solution. Once we have the WAL buffer,
		// we can just block here when enqueueing from it.
		samplesDropped.Inc()
	}
}
