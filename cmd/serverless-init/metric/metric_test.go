// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package metric

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer"
	"github.com/DataDog/datadog-agent/comp/aggregator/demultiplexer/demultiplexerimpl"
	"github.com/DataDog/datadog-agent/comp/core/hostname/hostnameimpl"
	log "github.com/DataDog/datadog-agent/comp/core/log/def"
	logmock "github.com/DataDog/datadog-agent/comp/core/log/mock"
	compressionmock "github.com/DataDog/datadog-agent/comp/serializer/compression/fx-mock"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
)

func TestAdd(t *testing.T) {
	demux := createDemultiplexer(t)
	timestamp := time.Now()
	add("a.super.metric", []string{"taga:valuea", "tagb:valueb"}, timestamp, demux)
	generatedMetrics, timedMetrics := demux.WaitForSamples(100 * time.Millisecond)
	assert.Equal(t, 0, len(timedMetrics))
	assert.Equal(t, 1, len(generatedMetrics))
	metric := generatedMetrics[0]
	assert.Equal(t, metric.Name, "a.super.metric")
	assert.Equal(t, 2, len(metric.Tags))
	assert.Equal(t, float64(timestamp.UnixNano())/float64(time.Second), metric.Timestamp)
	assert.Equal(t, metric.Tags[0], "taga:valuea")
	assert.Equal(t, metric.Tags[1], "tagb:valueb")
}

func TestAddColdStartMetric(t *testing.T) {
	demux := createDemultiplexer(t)
	timestamp := time.Now()
	AddColdStartMetric("gcp.run", []string{"taga:valuea", "tagb:valueb"}, timestamp, demux)
	generatedMetrics, timedMetrics := demux.WaitForSamples(100 * time.Millisecond)
	assert.Equal(t, 0, len(timedMetrics))
	assert.Equal(t, 1, len(generatedMetrics))
	metric := generatedMetrics[0]
	assert.Equal(t, metric.Name, "gcp.run.enhanced.cold_start")
	assert.Equal(t, 2, len(metric.Tags))
	assert.Equal(t, metric.Tags[0], "taga:valuea")
	assert.Equal(t, metric.Tags[1], "tagb:valueb")
}

func TestAddShutdownMetric(t *testing.T) {
	demux := createDemultiplexer(t)
	timestamp := time.Now()
	AddShutdownMetric("gcp.run", []string{"taga:valuea", "tagb:valueb"}, timestamp, demux)
	generatedMetrics, timedMetrics := demux.WaitForSamples(100 * time.Millisecond)
	assert.Equal(t, 0, len(timedMetrics))
	assert.Equal(t, 1, len(generatedMetrics))
	metric := generatedMetrics[0]
	assert.Equal(t, metric.Name, "gcp.run.enhanced.shutdown")
	assert.Equal(t, 2, len(metric.Tags))
	assert.Equal(t, metric.Tags[0], "taga:valuea")
	assert.Equal(t, metric.Tags[1], "tagb:valueb")
}

func TestNilDemuxDoesNotPanic(t *testing.T) {
	demux := createDemultiplexer(t)
	timestamp := time.Now()
	// Pass nil for demux to mimic when a port is blocked and dogstatsd does not start properly.
	// This previously led to a panic and segmentation fault
	add("metric", []string{"taga:valuea", "tagb:valueb"}, timestamp, nil)
	generatedMetrics, timedMetrics := demux.WaitForSamples(100 * time.Millisecond)
	assert.Equal(t, 0, len(timedMetrics))
	assert.Equal(t, 0, len(generatedMetrics))
}

func createDemultiplexer(t *testing.T) demultiplexer.FakeSamplerMock {
	return fxutil.Test[demultiplexer.FakeSamplerMock](t, fx.Provide(func() log.Component { return logmock.New(t) }), compressionmock.MockModule(), demultiplexerimpl.FakeSamplerMockModule(), hostnameimpl.MockModule())
}
