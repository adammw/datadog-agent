// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf

package http2

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKernelTelemetryUpdate(t *testing.T) {
	kernelTelemetryGroup := newHTTP2KernelTelemetry()

	// Populating values to simulate the eBPF map's HTTP2 telemetry
	http2Telemetry := &HTTP2Telemetry{
		Request_seen:                     5,
		Response_seen:                    5,
		End_of_stream:                    10,
		End_of_stream_rst:                11,
		Path_exceeds_frame:               20,
		Exceeding_max_interesting_frames: 30,
		Exceeding_max_frames_to_filter:   40,
		Path_size_bucket:                 [8]uint64{1, 2, 3, 4, 5, 6, 7, 8},
	}
	kernelTelemetryGroup.update(http2Telemetry)
	assertTelemetryEquality(t, http2Telemetry, kernelTelemetryGroup)

	// Increasing the values to simulate more data coming from the eBPF map's HTTP2 telemetry
	// This operation must be performed on the same object (without losing the pointer), as this aligns with
	// the expected behavior in the code
	http2Telemetry.Request_seen = 10
	http2Telemetry.Response_seen = 10
	http2Telemetry.End_of_stream = 11
	http2Telemetry.End_of_stream_rst = 18
	http2Telemetry.Path_exceeds_frame = 26
	http2Telemetry.Exceeding_max_interesting_frames = 32
	http2Telemetry.Exceeding_max_frames_to_filter = 42
	http2Telemetry.Path_size_bucket = [8]uint64{2, 3, 4, 5, 6, 7, 8, 9}
	kernelTelemetryGroup.update(http2Telemetry)
	assertTelemetryEquality(t, http2Telemetry, kernelTelemetryGroup)
}

func assertTelemetryEquality(t *testing.T, http2Telemetry *HTTP2Telemetry, kernelTelemetryGroup *kernelTelemetry) {
	assert.Equal(t, http2Telemetry.Request_seen, uint64(kernelTelemetryGroup.http2requests.Get()))
	assert.Equal(t, http2Telemetry.Response_seen, uint64(kernelTelemetryGroup.http2responses.Get()))
	assert.Equal(t, http2Telemetry.End_of_stream, uint64(kernelTelemetryGroup.endOfStream.Get()))
	assert.Equal(t, http2Telemetry.End_of_stream_rst, uint64(kernelTelemetryGroup.endOfStreamRST.Get()))
	assert.Equal(t, http2Telemetry.Path_exceeds_frame, uint64(kernelTelemetryGroup.pathExceedsFrame.Get()))
	assert.Equal(t, http2Telemetry.Exceeding_max_interesting_frames, uint64(kernelTelemetryGroup.exceedingMaxInterestingFrames.Get()))
	assert.Equal(t, http2Telemetry.Exceeding_max_frames_to_filter, uint64(kernelTelemetryGroup.exceedingMaxFramesToFilter.Get()))
	for i, bucket := range kernelTelemetryGroup.pathSizeBucket {
		assert.Equal(t, http2Telemetry.Path_size_bucket[i], uint64(bucket.Get()))
	}
}