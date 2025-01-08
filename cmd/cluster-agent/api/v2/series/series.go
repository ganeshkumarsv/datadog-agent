// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package series

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"

	"github.com/DataDog/agent-payload/v5/gogen"
	"github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/api"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/gorilla/mux"
)

const (
	encodingGzip           = "gzip"
	encodingDeflate        = "deflate"
	loadMetricsHandlerName = "load-metrics-handler"
)

// InstallNodeMetricsEndpoints register handler for node metrics collection
func InstallNodeMetricsEndpoints(ctx context.Context, r *mux.Router, cfg config.Component) {
	leaderHander := newSeriesHandler(ctx)
	handler := api.WithLeaderProxyHandler(
		loadMetricsHandlerName,
		func(w http.ResponseWriter, r *http.Request) bool { // preHandler
			if !cfg.GetBool("autoscaling.failover.enabled") {
				http.Error(w, "Autoscaling workload failover store is disabled on the cluster agent", http.StatusServiceUnavailable)
				return false
			}
			if r.Body == nil {
				http.Error(w, "Request body is empty", http.StatusBadRequest)
				return false
			}
			return true
		},
		leaderHander.handle,
	)
	r.HandleFunc("/series", api.WithTelemetryWrapper(loadMetricsHandlerName, handler)).Methods("POST")
}

// Handler handles the series request and store the metrics to loadstore
type seriesHandler struct {
	jobQueue *jobQueue
}

func newSeriesHandler(ctx context.Context) *seriesHandler {
	handler := seriesHandler{
		jobQueue: newJobQueue(ctx),
	}
	return &handler
}

func (h *seriesHandler) handle(w http.ResponseWriter, r *http.Request) {
	log.Tracef("Received series request from %s", r.RemoteAddr)
	var err error
	var rc io.ReadCloser
	switch r.Header.Get("Content-Encoding") {
	case encodingGzip:
		rc, err = gzip.NewReader(r.Body)
	case encodingDeflate:
		rc, err = zlib.NewReader(r.Body)
	default:
		rc = r.Body
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	payload, err := io.ReadAll(rc)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	metricPayload := &gogen.MetricPayload{}
	if err := metricPayload.Unmarshal(payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h.jobQueue.addJob(metricPayload)
	w.WriteHeader(http.StatusOK)
}
