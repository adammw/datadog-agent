// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package stats

import (
	"strings"
	"sync"
	"time"

	pb "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/log"
	"github.com/DataDog/datadog-agent/pkg/trace/traceutil"
	"github.com/DataDog/datadog-agent/pkg/trace/watchdog"

	"github.com/DataDog/datadog-go/v5/statsd"
)

// defaultBufferLen represents the default buffer length; the number of bucket size
// units used by the concentrator.
const defaultBufferLen = 2

// Writer is an interface for something that can Write Stats Payloads
type Writer interface {
	// Write this payload
	Write(*pb.StatsPayload)
}

// Concentrator produces time bucketed statistics from a stream of raw traces.
// https://en.wikipedia.org/wiki/Knelson_concentrator
// Gets an imperial shitton of traces, and outputs pre-computed data structures
// allowing to find the gold (stats) amongst the traces.
type Concentrator struct {
	Writer Writer

	spanConcentrator *SpanConcentrator
	// bucket duration in nanoseconds
	bsize         int64
	exit          chan struct{}
	exitWG        sync.WaitGroup
	agentEnv      string
	agentHostname string
	agentVersion  string
	statsd        statsd.ClientInterface
}

// NewConcentrator initializes a new concentrator ready to be started
func NewConcentrator(conf *config.AgentConfig, writer Writer, now time.Time, statsd statsd.ClientInterface) *Concentrator {
	bsize := conf.BucketInterval.Nanoseconds()
	sc := NewSpanConcentrator(&SpanConcentratorConfig{
		ComputeStatsBySpanKind: conf.ComputeStatsBySpanKind,
		BucketInterval:         bsize,
		PeerTags:               conf.ConfiguredPeerTags(),
	}, now)
	c := Concentrator{
		spanConcentrator: sc,
		Writer:           writer,
		exit:             make(chan struct{}),
		agentEnv:         conf.DefaultEnv,
		agentHostname:    conf.Hostname,
		agentVersion:     conf.AgentVersion,
		statsd:           statsd,
		bsize:            bsize,
	}
	return &c
}

// Start starts the concentrator.
func (c *Concentrator) Start() {
	c.exitWG.Add(1)
	go func() {
		defer watchdog.LogOnPanic(c.statsd)
		defer c.exitWG.Done()
		c.Run()
	}()
}

// Run runs the main loop of the concentrator goroutine. Traces are received
// through `Add`, this loop only deals with flushing.
func (c *Concentrator) Run() {
	// flush with the same period as stats buckets
	flushTicker := time.NewTicker(time.Duration(c.bsize) * time.Nanosecond)
	defer flushTicker.Stop()

	log.Debug("Starting concentrator")

	for {
		select {
		case <-flushTicker.C:
			c.Writer.Write(c.Flush(false))
		case <-c.exit:
			log.Info("Exiting concentrator, computing remaining stats")
			c.Writer.Write(c.Flush(true))
			return
		}
	}
}

// Stop stops the main Run loop.
func (c *Concentrator) Stop() {
	close(c.exit)
	c.exitWG.Wait()
}

// computeStatsForSpanKind returns true if the span.kind value makes the span eligible for stats computation.
func computeStatsForSpanKind(s *pb.Span) bool {
	k := strings.ToLower(s.Meta["span.kind"])
	switch k {
	case "server", "consumer", "client", "producer":
		return true
	default:
		return false
	}
}

// Input specifies a set of traces originating from a certain payload.
type Input struct {
	Traces        []traceutil.ProcessedTrace
	ContainerID   string
	ContainerTags []string
}

// NewStatsInput allocates a stats input for an incoming trace payload
func NewStatsInput(numChunks int, containerID string, clientComputedStats bool, conf *config.AgentConfig) Input {
	if clientComputedStats {
		return Input{}
	}
	in := Input{Traces: make([]traceutil.ProcessedTrace, 0, numChunks)}
	_, enabledCIDStats := conf.Features["enable_cid_stats"]
	_, disabledCIDStats := conf.Features["disable_cid_stats"]
	enableContainers := enabledCIDStats || (conf.FargateOrchestrator != config.OrchestratorUnknown)
	if enableContainers && !disabledCIDStats {
		// only allow the ContainerID stats dimension if we're in a Fargate instance or it's
		// been explicitly enabled and it's not prohibited by the disable_cid_stats feature flag.
		in.ContainerID = containerID
	}
	return in
}

// Add applies the given input to the concentrator.
func (c *Concentrator) Add(t Input) {
	for _, trace := range t.Traces {
		c.addNow(&trace, t.ContainerID, t.ContainerTags)
	}
}

// addNow adds the given input into the concentrator.
// Callers must guard!
func (c *Concentrator) addNow(pt *traceutil.ProcessedTrace, containerID string, containerTags []string) {
	hostname := pt.TracerHostname
	if hostname == "" {
		hostname = c.agentHostname
	}
	env := pt.TracerEnv
	if env == "" {
		env = c.agentEnv
	}
	weight := weight(pt.Root)
	aggKey := PayloadAggregationKey{
		Env:          env,
		Hostname:     hostname,
		Version:      pt.AppVersion,
		ContainerID:  containerID,
		GitCommitSha: pt.GitCommitSha,
		ImageTag:     pt.ImageTag,
	}
	for _, s := range pt.TraceChunk.Spans {
		c.spanConcentrator.addSpan(s, aggKey, containerID, containerTags, pt.TraceChunk.Origin, weight)
	}
}

// Flush deletes and returns complete statistic buckets.
// The force boolean guarantees flushing all buckets if set to true.
func (c *Concentrator) Flush(force bool) *pb.StatsPayload {
	return c.flushNow(time.Now().UnixNano(), force)
}

func (c *Concentrator) flushNow(now int64, force bool) *pb.StatsPayload {
	sb := c.spanConcentrator.Flush(now, force)
	return &pb.StatsPayload{Stats: sb, AgentHostname: c.agentHostname, AgentEnv: c.agentEnv, AgentVersion: c.agentVersion}
}

// alignTs returns the provided timestamp truncated to the bucket size.
// It gives us the start time of the time bucket in which such timestamp falls.
func alignTs(ts int64, bsize int64) int64 {
	return ts - ts%bsize
}
