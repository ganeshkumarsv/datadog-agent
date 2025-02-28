// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux

// Origin detection is linux-only

// Most of it is tested by test/integration/dogstatsd/origin_detection_test.go
// that requires a docker environment to run

package listeners

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/sys/unix"

	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	"github.com/DataDog/datadog-agent/comp/dogstatsd/packets"
	"github.com/DataDog/datadog-agent/pkg/util/option"
)

func TestUDSPassCred(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "dsd.socket")

	cfg := map[string]interface{}{}
	cfg["dogstatsd_socket"] = socketPath
	cfg["dogstatsd_origin_detection"] = true

	deps := fulfillDepsWithConfig(t, cfg)
	packetsTelemetryStore := packets.NewTelemetryStore(nil, deps.Telemetry)
	listernersTelemetryStore := NewTelemetryStore(nil, deps.Telemetry)
	pool := packets.NewPool(512, packetsTelemetryStore)
	poolManager := packets.NewPoolManager(pool)
	s, err := NewUDSDatagramListener(nil, poolManager, nil, deps.Config, nil, option.None[workloadmeta.Component](), deps.PidMap, listernersTelemetryStore, packetsTelemetryStore, deps.Telemetry)
	defer s.Stop()

	assert.Nil(t, err)
	assert.NotNil(t, s)

	// Test socket has PASSCRED option set to 1
	f, err := s.conn.File()
	require.Nil(t, err)
	defer f.Close()

	enabled, err := unix.GetsockoptInt(int(f.Fd()), unix.SOL_SOCKET, unix.SO_PASSCRED)
	assert.Nil(t, err)
	assert.Equal(t, enabled, 1)
}
