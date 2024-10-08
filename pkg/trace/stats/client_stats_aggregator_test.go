// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package stats

import (
	"sync"
	"testing"
	"time"

	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/runtime/protoiface"

	proto "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/pb"

	"github.com/DataDog/datadog-go/v5/statsd"
)

var fuzzer = fuzz.NewWithSeed(1)

func newTestAggregator() *ClientStatsAggregator {
	conf := &config.AgentConfig{
		DefaultEnv: "agentEnv",
		Hostname:   "agentHostname",
	}
	a := NewClientStatsAggregator(conf, noopStatsWriter{}, &statsd.NoOpClient{})
	a.Start()
	a.flushTicker.Stop()
	return a
}

type noopStatsWriter struct{}

func (noopStatsWriter) Write(*proto.StatsPayload) {}

type mockStatsWriter struct {
	payloads []*proto.StatsPayload
	mu       sync.Mutex
}

func (w *mockStatsWriter) Write(p *proto.StatsPayload) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.payloads = append(w.payloads, p)
}

func (w *mockStatsWriter) Reset() []*proto.StatsPayload {
	w.mu.Lock()
	defer w.mu.Unlock()
	ret := w.payloads
	w.payloads = nil
	return ret
}

func wrapPayload(p *proto.ClientStatsPayload) *proto.StatsPayload {
	return wrapPayloads([]*proto.ClientStatsPayload{p})
}

func wrapPayloads(p []*proto.ClientStatsPayload) *proto.StatsPayload {
	return &proto.StatsPayload{
		AgentEnv:       "agentEnv",
		AgentHostname:  "agentHostname",
		ClientComputed: true,
		Stats:          p,
	}
}

func payloadWithCounts(ts time.Time, k BucketsAggregationKey, containerID, version, imageTag, gitCommitSha string, hits, errors, duration uint64) *proto.ClientStatsPayload {
	return &proto.ClientStatsPayload{
		Env:          "test-env",
		Version:      version,
		ImageTag:     imageTag,
		GitCommitSha: gitCommitSha,
		ContainerID:  containerID,
		Stats: []*proto.ClientStatsBucket{
			{
				Start: uint64(ts.UnixNano()),
				Stats: []*proto.ClientGroupedStats{
					{
						Service:        k.Service,
						Name:           k.Name,
						SpanKind:       k.SpanKind,
						Resource:       k.Resource,
						HTTPStatusCode: k.StatusCode,
						Type:           k.Type,
						Synthetics:     k.Synthetics,
						Hits:           hits,
						Errors:         errors,
						Duration:       duration,
					},
				},
			},
		},
	}
}

func getTestStatsWithStart(start time.Time) *proto.ClientStatsPayload {
	b := &proto.ClientStatsBucket{}
	fuzzer.Fuzz(b)
	b.Start = uint64(start.UnixNano())
	p := &proto.ClientStatsPayload{}
	fuzzer.Fuzz(p)
	p.Tags = nil
	p.Stats = []*proto.ClientStatsBucket{b}
	return p
}

func assertDistribPayload(t *testing.T, withCounts, res *proto.StatsPayload) {
	for j, p := range withCounts.Stats {
		withCounts.Stats[j].AgentAggregation = keyDistributions
		for _, s := range p.Stats {
			for i := range s.Stats {
				if s.Stats[i] == nil {
					continue
				}
				s.Stats[i].Hits = 0
				s.Stats[i].Errors = 0
				s.Stats[i].Duration = 0
			}
		}
	}
	assert.Equal(t, withCounts.String(), res.String())
}

func assertAggCountsPayload(t *testing.T, aggCounts *proto.StatsPayload) {
	for _, p := range aggCounts.Stats {
		assert.Empty(t, p.Lang)
		assert.Empty(t, p.TracerVersion)
		assert.Empty(t, p.RuntimeID)
		assert.Equal(t, uint64(0), p.Sequence)
		assert.Equal(t, keyCounts, p.AgentAggregation)
		for _, s := range p.Stats {
			for _, b := range s.Stats {
				assert.Nil(t, b.OkSummary)
				assert.Nil(t, b.ErrorSummary)
			}
		}
	}
}

func agg2Counts(insertionTime time.Time, p *proto.ClientStatsPayload) *proto.ClientStatsPayload {
	p.Lang = ""
	p.TracerVersion = ""
	p.RuntimeID = ""
	p.Sequence = 0
	p.AgentAggregation = "counts"
	p.Service = ""
	p.ContainerID = ""
	for _, s := range p.Stats {
		s.Start = uint64(alignAggTs(insertionTime).UnixNano())
		s.Duration = uint64(clientBucketDuration.Nanoseconds())
		s.AgentTimeShift = 0
		for _, stat := range s.Stats {
			if stat == nil {
				continue
			}
			stat.DBType = ""
			stat.Hits *= 2
			stat.Errors *= 2
			stat.Duration *= 2
			stat.TopLevelHits = 0
			stat.OkSummary = nil
			stat.ErrorSummary = nil
		}
	}
	return p
}

func TestAggregatorFlushTime(t *testing.T) {
	assert := assert.New(t)
	a := newTestAggregator()
	msw := &mockStatsWriter{}
	a.writer = msw
	testTime := time.Now()
	a.flushOnTime(testTime)
	assert.Len(msw.payloads, 0)
	testPayload := getTestStatsWithStart(testTime)
	a.add(testTime, deepCopy(testPayload))
	a.flushOnTime(testTime)
	assert.Len(msw.payloads, 0)
	a.flushOnTime(testTime.Add(oldestBucketStart - bucketDuration))
	assert.Len(msw.payloads, 0)
	a.flushOnTime(testTime.Add(oldestBucketStart))
	require.NotEmpty(t, msw.payloads)
	s := msw.payloads[0]
	assert.Equal(s.String(), wrapPayload(testPayload).String())
	assert.Len(a.buckets, 0)
}

func TestMergeMany(t *testing.T) {
	assert := assert.New(t)
	for i := 0; i < 10; i++ {
		a := newTestAggregator()
		msw := &mockStatsWriter{}
		a.writer = msw
		payloadTime := time.Now().Truncate(bucketDuration)
		merge1 := getTestStatsWithStart(payloadTime)
		merge2 := getTestStatsWithStart(payloadTime.Add(time.Nanosecond))
		other := getTestStatsWithStart(payloadTime.Add(-time.Nanosecond))
		merge3 := getTestStatsWithStart(payloadTime.Add(time.Second - time.Nanosecond))

		insertionTime := payloadTime.Add(time.Second)
		a.add(insertionTime, deepCopy(merge1))
		a.add(insertionTime, deepCopy(merge2))
		a.add(insertionTime, deepCopy(other))
		a.add(insertionTime, deepCopy(merge3))
		assert.Len(msw.payloads, 2)
		a.flushOnTime(payloadTime.Add(oldestBucketStart - time.Nanosecond))
		assert.Len(msw.payloads, 3)
		a.flushOnTime(payloadTime.Add(oldestBucketStart))
		assert.Len(msw.payloads, 4)
		assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{merge1, merge2}), msw.payloads[0])
		assertDistribPayload(t, wrapPayload(merge3), msw.payloads[1])
		s := msw.payloads[2]
		assert.Equal(wrapPayload(other).String(), s.String())
		assertAggCountsPayload(t, msw.payloads[3])
		assert.Len(a.buckets, 0)
	}
}

func TestConcentratorAggregatorNotAligned(t *testing.T) {
	var ts time.Time
	bsize := clientBucketDuration.Nanoseconds()
	for i := 0; i < 50; i++ {
		fuzzer.Fuzz(&ts)
		aggTs := alignAggTs(ts)
		assert.True(t, aggTs.UnixNano()%bsize != 0)
		concentratorTs := alignTs(ts.UnixNano(), bsize)
		assert.True(t, concentratorTs%bsize == 0)
	}
}

func TestTimeShifts(t *testing.T) {
	type tt struct {
		shift, expectedShift time.Duration
		name                 string
	}
	tts := []tt{
		{
			shift:         100 * time.Hour,
			expectedShift: 100 * time.Hour,
			name:          "future",
		},
		{
			shift:         -11 * time.Hour,
			expectedShift: -11*time.Hour + oldestBucketStart - bucketDuration,
			name:          "past",
		},
	}
	for _, tc := range tts {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			a := newTestAggregator()
			msw := &mockStatsWriter{}
			a.writer = msw
			agentTime := alignAggTs(time.Now())
			payloadTime := agentTime.Add(tc.shift)

			stats := getTestStatsWithStart(payloadTime)
			a.add(agentTime, deepCopy(stats))
			a.flushOnTime(agentTime)
			assert.Len(msw.payloads, 0)
			a.flushOnTime(agentTime.Add(oldestBucketStart + time.Nanosecond))
			require.Len(t, msw.payloads, 1)
			stats.Stats[0].AgentTimeShift = -tc.expectedShift.Nanoseconds()
			stats.Stats[0].Start -= uint64(tc.expectedShift.Nanoseconds())
			s := msw.payloads[0]
			assert.Equal(wrapPayload(stats).String(), s.String())
		})
	}
}

func TestFuzzCountFields(t *testing.T) {
	assert := assert.New(t)
	for i := 0; i < 30; i++ {
		a := newTestAggregator()
		msw := &mockStatsWriter{}
		a.writer = msw
		// Ensure that peer tags aggregation is on. Some tests may expect non-empty values the peer tags.
		payloadTime := time.Now().Truncate(bucketDuration)
		merge1 := getTestStatsWithStart(payloadTime)

		insertionTime := payloadTime.Add(time.Second)
		a.add(insertionTime, deepCopy(merge1))
		a.add(insertionTime, deepCopy(merge1))
		assert.Len(msw.payloads, 1)
		a.flushOnTime(payloadTime.Add(oldestBucketStart))
		require.Len(t, msw.payloads, 2)
		assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{deepCopy(merge1), deepCopy(merge1)}), msw.payloads[0])
		aggCounts := msw.payloads[1]
		expectedAggCounts := wrapPayload(agg2Counts(insertionTime, merge1))

		// map gives random orders post aggregation

		actual := []protoiface.MessageV1{}
		expected := []protoiface.MessageV1{}
		for _, s := range expectedAggCounts.Stats[0].Stats[0].Stats {
			if s == nil {
				continue
			}
			expected = append(expected, s)
		}
		for _, s := range aggCounts.Stats[0].Stats[0].Stats {
			if s == nil {
				continue
			}
			actual = append(actual, s)
		}

		assert.ElementsMatch(pb.ToStringSlice(expected), pb.ToStringSlice(actual))
		aggCounts.Stats[0].Stats[0].Stats = nil
		expectedAggCounts.Stats[0].Stats[0].Stats = nil
		assert.Equal(expectedAggCounts, aggCounts)
		assert.Len(a.buckets, 0)
	}
}

func TestCountAggregation(t *testing.T) {
	assert := assert.New(t)
	type tt struct {
		k    BucketsAggregationKey
		res  *proto.ClientGroupedStats
		name string
	}
	tts := []tt{
		{
			BucketsAggregationKey{Service: "s"},
			&proto.ClientGroupedStats{Service: "s"},
			"service",
		},
		{
			BucketsAggregationKey{Name: "n"},
			&proto.ClientGroupedStats{Name: "n"},
			"name",
		},
		{
			BucketsAggregationKey{Resource: "r"},
			&proto.ClientGroupedStats{Resource: "r"},
			"resource",
		},
		{
			BucketsAggregationKey{Type: "t"},
			&proto.ClientGroupedStats{Type: "t"},
			"resource",
		},
		{
			BucketsAggregationKey{Synthetics: true},
			&proto.ClientGroupedStats{Synthetics: true},
			"synthetics",
		},
		{
			BucketsAggregationKey{StatusCode: 10},
			&proto.ClientGroupedStats{HTTPStatusCode: 10},
			"status",
		},
	}
	for _, tc := range tts {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAggregator()
			msw := &mockStatsWriter{}
			a.writer = msw
			testTime := time.Unix(time.Now().Unix(), 0)

			c1 := payloadWithCounts(testTime, tc.k, "", "test-version", "", "", 11, 7, 100)
			c2 := payloadWithCounts(testTime, tc.k, "", "test-version", "", "", 27, 2, 300)
			c3 := payloadWithCounts(testTime, tc.k, "", "test-version", "", "", 5, 10, 3)
			keyDefault := BucketsAggregationKey{}
			cDefault := payloadWithCounts(testTime, keyDefault, "", "test-version", "", "", 0, 2, 4)

			assert.Len(msw.payloads, 0)
			a.add(testTime, deepCopy(c1))
			a.add(testTime, deepCopy(c2))
			a.add(testTime, deepCopy(c3))
			a.add(testTime, deepCopy(cDefault))
			assert.Len(msw.payloads, 3)
			a.flushOnTime(testTime.Add(oldestBucketStart + time.Nanosecond))
			require.Len(t, msw.payloads, 4)

			assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{c1, c2}), msw.payloads[0])
			assertDistribPayload(t, wrapPayload(c3), msw.payloads[1])
			assertDistribPayload(t, wrapPayload(cDefault), msw.payloads[2])
			aggCounts := msw.payloads[3]
			assertAggCountsPayload(t, aggCounts)

			tc.res.Hits = 43
			tc.res.Errors = 19
			tc.res.Duration = 403
			assert.ElementsMatch(aggCounts.Stats[0].Stats[0].Stats, []*proto.ClientGroupedStats{
				tc.res,
				// Additional grouped stat object that corresponds to the keyDefault/cDefault.
				// We do not expect this to be aggregated with the non-default key in the test.
				{
					Hits:     0,
					Errors:   2,
					Duration: 4,
				},
			})
			assert.Len(a.buckets, 0)
		})
	}
}

func TestCountAggregationPeerTags(t *testing.T) {
	type tt struct {
		k        BucketsAggregationKey
		res      *proto.ClientGroupedStats
		name     string
		peerTags []string
	}
	// The fnv64a hash of the peerTags var.
	peerTagsHash := uint64(8580633704111928789)
	peerTags := []string{"db.instance:a", "db.system:b", "peer.service:remote-service"}
	tts := []tt{
		{
			BucketsAggregationKey{Service: "s", Name: "test.op"},
			&proto.ClientGroupedStats{Service: "s", Name: "test.op"},
			"peer tags aggregation disabled",
			nil,
		},
		{
			BucketsAggregationKey{Service: "s", PeerTagsHash: peerTagsHash},
			&proto.ClientGroupedStats{Service: "s", PeerTags: peerTags},
			"peer tags aggregation enabled",
			peerTags,
		},
	}
	for _, tc := range tts {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			a := newTestAggregator()
			msw := &mockStatsWriter{}
			a.writer = msw
			testTime := time.Unix(time.Now().Unix(), 0)

			c1 := payloadWithCounts(testTime, tc.k, "", "test-version", "", "", 11, 7, 100)
			c2 := payloadWithCounts(testTime, tc.k, "", "test-version", "", "", 27, 2, 300)
			c3 := payloadWithCounts(testTime, tc.k, "", "test-version", "", "", 5, 10, 3)
			c1.Stats[0].Stats[0].PeerTags = tc.peerTags
			c2.Stats[0].Stats[0].PeerTags = tc.peerTags
			c3.Stats[0].Stats[0].PeerTags = tc.peerTags
			keyDefault := BucketsAggregationKey{}
			cDefault := payloadWithCounts(testTime, keyDefault, "", "test-version", "", "", 0, 2, 4)

			assert.Len(msw.payloads, 0)
			a.add(testTime, deepCopy(c1))
			a.add(testTime, deepCopy(c2))
			a.add(testTime, deepCopy(c3))
			a.add(testTime, deepCopy(cDefault))
			assert.Len(msw.payloads, 3)
			a.flushOnTime(testTime.Add(oldestBucketStart + time.Nanosecond))
			require.Len(t, msw.payloads, 4)

			assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{c1, c2}), msw.payloads[0])
			assertDistribPayload(t, wrapPayload(c3), msw.payloads[1])
			assertDistribPayload(t, wrapPayload(cDefault), msw.payloads[2])
			aggCounts := msw.payloads[3]
			assertAggCountsPayload(t, aggCounts)

			tc.res.Hits = 43
			tc.res.Errors = 19
			tc.res.Duration = 403
			assert.ElementsMatch(aggCounts.Stats[0].Stats[0].Stats, []*proto.ClientGroupedStats{
				tc.res,
				// Additional grouped stat object that corresponds to the keyDefault/cDefault.
				// We do not expect this to be aggregated with the non-default key in the test.
				{
					Hits:     0,
					Errors:   2,
					Duration: 4,
				},
			})
			assert.Len(a.buckets, 0)
		})
	}
}

func TestAggregationVersionData(t *testing.T) {
	// Version data refers to all of: Version, GitCommitSha, and ImageTag.
	t.Run("all version data provided in payload", func(t *testing.T) {
		assert := assert.New(t)
		a := newTestAggregator()
		msw := &mockStatsWriter{}
		a.writer = msw
		testTime := time.Unix(time.Now().Unix(), 0)

		bak := BucketsAggregationKey{Service: "s", Name: "test.op"}
		c1 := payloadWithCounts(testTime, bak, "1", "test-version", "abc", "abc123", 11, 7, 100)
		c2 := payloadWithCounts(testTime, bak, "1", "test-version", "abc", "abc123", 27, 2, 300)
		c3 := payloadWithCounts(testTime, bak, "1", "test-version", "abc", "abc123", 5, 10, 3)
		keyDefault := BucketsAggregationKey{}
		cDefault := payloadWithCounts(testTime, keyDefault, "1", "test-version", "abc", "abc123", 0, 2, 4)

		assert.Len(msw.payloads, 0)
		a.add(testTime, deepCopy(c1))
		a.add(testTime, deepCopy(c2))
		a.add(testTime, deepCopy(c3))
		a.add(testTime, deepCopy(cDefault))
		assert.Len(msw.payloads, 3)
		a.flushOnTime(testTime.Add(oldestBucketStart + time.Nanosecond))
		require.Len(t, msw.payloads, 4)

		assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{c1, c2}), msw.payloads[0])
		assertDistribPayload(t, wrapPayload(c3), msw.payloads[1])
		assertDistribPayload(t, wrapPayload(cDefault), msw.payloads[2])
		aggCounts := msw.payloads[3]
		assertAggCountsPayload(t, aggCounts)

		expectedRes := &proto.ClientGroupedStats{
			Service:  bak.Service,
			Name:     bak.Name,
			Hits:     43,
			Errors:   19,
			Duration: 403,
		}
		assert.ElementsMatch(aggCounts.Stats[0].Stats[0].Stats, []*proto.ClientGroupedStats{
			expectedRes,
			// Additional grouped stat object that corresponds to the keyDefault/cDefault.
			// We do not expect this to be aggregated with the non-default key in the test.
			{
				Hits:     0,
				Errors:   2,
				Duration: 4,
			},
		})
		assert.Equal("test-version", aggCounts.Stats[0].Version)
		assert.Equal("abc", aggCounts.Stats[0].ImageTag)
		assert.Equal("abc123", aggCounts.Stats[0].GitCommitSha)
		assert.Len(a.buckets, 0)
	})

	t.Run("git commit sha and image tag come from container tags", func(t *testing.T) {
		assert := assert.New(t)
		a := newTestAggregator()
		msw := &mockStatsWriter{}
		a.writer = msw
		cfg := config.New()
		cfg.ContainerTags = func(_ string) ([]string, error) {
			return []string{"git.commit.sha:sha-from-container-tags", "image_tag:image-tag-from-container-tags"}, nil
		}
		a.conf = cfg
		testTime := time.Unix(time.Now().Unix(), 0)

		bak := BucketsAggregationKey{Service: "s", Name: "test.op"}
		c1 := payloadWithCounts(testTime, bak, "1", "", "", "", 11, 7, 100)
		c2 := payloadWithCounts(testTime, bak, "1", "", "", "", 27, 2, 300)
		c3 := payloadWithCounts(testTime, bak, "1", "", "", "", 5, 10, 3)
		keyDefault := BucketsAggregationKey{}
		cDefault := payloadWithCounts(testTime, keyDefault, "1", "", "", "", 0, 2, 4)

		assert.Len(msw.payloads, 0)
		a.add(testTime, deepCopy(c1))
		a.add(testTime, deepCopy(c2))
		a.add(testTime, deepCopy(c3))
		a.add(testTime, deepCopy(cDefault))
		assert.Len(msw.payloads, 3)
		a.flushOnTime(testTime.Add(oldestBucketStart + time.Nanosecond))
		require.Len(t, msw.payloads, 4)

		// Add the expected gitCommitSha and imageTag on c1, c2, c3, and cDefault for these assertions.
		c1.GitCommitSha = "sha-from-container-tags"
		c1.ImageTag = "image-tag-from-container-tags"
		c2.GitCommitSha = "sha-from-container-tags"
		c2.ImageTag = "image-tag-from-container-tags"
		c3.GitCommitSha = "sha-from-container-tags"
		c3.ImageTag = "image-tag-from-container-tags"
		cDefault.GitCommitSha = "sha-from-container-tags"
		cDefault.ImageTag = "image-tag-from-container-tags"
		assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{c1, c2}), msw.payloads[0])
		assertDistribPayload(t, wrapPayload(c3), msw.payloads[1])
		assertDistribPayload(t, wrapPayload(cDefault), msw.payloads[2])
		aggCounts := msw.payloads[3]
		assertAggCountsPayload(t, aggCounts)

		expectedRes := &proto.ClientGroupedStats{
			Service:  bak.Service,
			Name:     bak.Name,
			Hits:     43,
			Errors:   19,
			Duration: 403,
		}
		assert.ElementsMatch(aggCounts.Stats[0].Stats[0].Stats, []*proto.ClientGroupedStats{
			expectedRes,
			// Additional grouped stat object that corresponds to the keyDefault/cDefault.
			// We do not expect this to be aggregated with the non-default key in the test.
			{
				Hits:     0,
				Errors:   2,
				Duration: 4,
			},
		})
		assert.Equal("", aggCounts.Stats[0].Version)
		assert.Equal("image-tag-from-container-tags", aggCounts.Stats[0].ImageTag)
		assert.Equal("sha-from-container-tags", aggCounts.Stats[0].GitCommitSha)
		assert.Len(a.buckets, 0)
	})

	t.Run("payload git commit sha and image tag override container tags", func(t *testing.T) {
		assert := assert.New(t)
		a := newTestAggregator()
		msw := &mockStatsWriter{}
		a.writer = msw
		cfg := config.New()
		cfg.ContainerTags = func(_ string) ([]string, error) {
			return []string{"git.commit.sha:overrideThisSha", "image_tag:overrideThisImageTag"}, nil
		}
		a.conf = cfg
		testTime := time.Unix(time.Now().Unix(), 0)

		bak := BucketsAggregationKey{Service: "s", Name: "test.op"}
		c1 := payloadWithCounts(testTime, bak, "1", "test-version", "abc", "abc123", 11, 7, 100)
		c2 := payloadWithCounts(testTime, bak, "1", "test-version", "abc", "abc123", 27, 2, 300)
		c3 := payloadWithCounts(testTime, bak, "1", "test-version", "abc", "abc123", 5, 10, 3)
		keyDefault := BucketsAggregationKey{}
		cDefault := payloadWithCounts(testTime, keyDefault, "1", "test-version", "abc", "abc123", 0, 2, 4)

		assert.Len(msw.payloads, 0)
		a.add(testTime, deepCopy(c1))
		a.add(testTime, deepCopy(c2))
		a.add(testTime, deepCopy(c3))
		a.add(testTime, deepCopy(cDefault))
		assert.Len(msw.payloads, 3)
		a.flushOnTime(testTime.Add(oldestBucketStart + time.Nanosecond))
		require.Len(t, msw.payloads, 4)

		assertDistribPayload(t, wrapPayloads([]*proto.ClientStatsPayload{c1, c2}), msw.payloads[0])
		assertDistribPayload(t, wrapPayload(c3), msw.payloads[1])
		assertDistribPayload(t, wrapPayload(cDefault), msw.payloads[2])
		aggCounts := msw.payloads[3]
		assertAggCountsPayload(t, aggCounts)

		expectedRes := &proto.ClientGroupedStats{
			Service:  bak.Service,
			Name:     bak.Name,
			Hits:     43,
			Errors:   19,
			Duration: 403,
		}
		assert.ElementsMatch(aggCounts.Stats[0].Stats[0].Stats, []*proto.ClientGroupedStats{
			expectedRes,
			// Additional grouped stat object that corresponds to the keyDefault/cDefault.
			// We do not expect this to be aggregated with the non-default key in the test.
			{
				Hits:     0,
				Errors:   2,
				Duration: 4,
			},
		})
		assert.Equal("test-version", aggCounts.Stats[0].Version)
		assert.Equal("abc", aggCounts.Stats[0].ImageTag)
		assert.Equal("abc123", aggCounts.Stats[0].GitCommitSha)
		assert.Len(a.buckets, 0)
	})
}

func TestNewBucketAggregationKeyPeerTags(t *testing.T) {
	// The hash of "peer.service:remote-service".
	peerTagsHash := uint64(3430395298086625290)
	t.Run("disabled", func(t *testing.T) {
		assert := assert.New(t)
		r := newBucketAggregationKey(&proto.ClientGroupedStats{Service: "a"})
		assert.Equal(BucketsAggregationKey{Service: "a"}, r)
	})
	t.Run("enabled", func(t *testing.T) {
		assert := assert.New(t)
		r := newBucketAggregationKey(&proto.ClientGroupedStats{Service: "a", PeerTags: []string{"peer.service:remote-service"}})
		assert.Equal(BucketsAggregationKey{Service: "a", PeerTagsHash: peerTagsHash}, r)
	})
}

func deepCopy(p *proto.ClientStatsPayload) *proto.ClientStatsPayload {
	payload := &proto.ClientStatsPayload{
		Hostname:         p.GetHostname(),
		Env:              p.GetEnv(),
		Version:          p.GetVersion(),
		Lang:             p.GetLang(),
		TracerVersion:    p.GetTracerVersion(),
		RuntimeID:        p.GetRuntimeID(),
		Sequence:         p.GetSequence(),
		AgentAggregation: p.GetAgentAggregation(),
		Service:          p.GetService(),
		ContainerID:      p.GetContainerID(),
		Tags:             p.GetTags(),
		GitCommitSha:     p.GetGitCommitSha(),
		ImageTag:         p.GetImageTag(),
	}
	payload.Stats = deepCopyStatsBucket(p.Stats)
	return payload
}

func deepCopyStatsBucket(s []*proto.ClientStatsBucket) []*proto.ClientStatsBucket {
	if s == nil {
		return nil
	}
	bucket := make([]*proto.ClientStatsBucket, len(s))
	for i, b := range s {
		bucket[i] = &proto.ClientStatsBucket{
			Start:          b.GetStart(),
			Duration:       b.GetDuration(),
			AgentTimeShift: b.GetAgentTimeShift(),
		}
		bucket[i].Stats = deepCopyGroupedStats(b.Stats)
	}
	return bucket
}

func deepCopyGroupedStats(s []*proto.ClientGroupedStats) []*proto.ClientGroupedStats {
	if s == nil {
		return nil
	}
	stats := make([]*proto.ClientGroupedStats, len(s))
	for i, b := range s {
		if b == nil {
			stats[i] = nil
			continue
		}

		stats[i] = &proto.ClientGroupedStats{
			Service:        b.GetService(),
			Name:           b.GetName(),
			Resource:       b.GetResource(),
			HTTPStatusCode: b.GetHTTPStatusCode(),
			Type:           b.GetType(),
			DBType:         b.GetDBType(),
			Hits:           b.GetHits(),
			Errors:         b.GetErrors(),
			Duration:       b.GetDuration(),
			Synthetics:     b.GetSynthetics(),
			TopLevelHits:   b.GetTopLevelHits(),
			SpanKind:       b.GetSpanKind(),
			PeerTags:       b.GetPeerTags(),
			IsTraceRoot:    b.GetIsTraceRoot(),
		}
		if b.OkSummary != nil {
			stats[i].OkSummary = make([]byte, len(b.OkSummary))
			copy(stats[i].OkSummary, b.OkSummary)
		}
		if b.ErrorSummary != nil {
			stats[i].ErrorSummary = make([]byte, len(b.ErrorSummary))
			copy(stats[i].ErrorSummary, b.ErrorSummary)
		}
	}
	return stats
}
