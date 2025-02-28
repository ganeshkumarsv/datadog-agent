// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux

// Package dump holds dump related files
package dump

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"go.uber.org/atomic"

	"github.com/DataDog/datadog-go/v5/statsd"

	logsconfig "github.com/DataDog/datadog-agent/comp/logs/agent/config"
	pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"
	"github.com/DataDog/datadog-agent/pkg/security/config"
	"github.com/DataDog/datadog-agent/pkg/security/metrics"
	"github.com/DataDog/datadog-agent/pkg/security/seclog"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
	ddhttputil "github.com/DataDog/datadog-agent/pkg/util/http"
)

type tooLargeEntityStatsEntry struct {
	storageFormat config.StorageFormat
	compression   bool
}

type remoteEndpoint struct {
	logsEndpoint logsconfig.Endpoint
	url          string
}

// ActivityDumpRemoteStorage is a remote storage that forwards dumps to the backend
type ActivityDumpRemoteStorage struct {
	endpoints        []remoteEndpoint
	tooLargeEntities map[tooLargeEntityStatsEntry]*atomic.Uint64

	client *http.Client
}

// NewActivityDumpRemoteStorage returns a new instance of ActivityDumpRemoteStorage
func NewActivityDumpRemoteStorage() (ActivityDumpStorage, error) {
	storage := &ActivityDumpRemoteStorage{
		tooLargeEntities: make(map[tooLargeEntityStatsEntry]*atomic.Uint64),
		client: &http.Client{
			Transport: ddhttputil.CreateHTTPTransport(pkgconfigsetup.Datadog()),
		},
	}

	for _, format := range config.AllStorageFormats() {
		for _, compression := range []bool{true, false} {
			entry := tooLargeEntityStatsEntry{
				storageFormat: format,
				compression:   compression,
			}
			storage.tooLargeEntities[entry] = atomic.NewUint64(0)
		}
	}

	endpoints, err := config.ActivityDumpRemoteStorageEndpoints("cws-intake.", "secdump", logsconfig.DefaultIntakeProtocol, "cloud-workload-security")
	if err != nil {
		return nil, fmt.Errorf("couldn't generate storage endpoints: %w", err)
	}
	for _, endpoint := range endpoints.GetReliableEndpoints() {
		storage.endpoints = append(storage.endpoints, remoteEndpoint{
			logsEndpoint: endpoint,
			url:          utils.GetEndpointURL(endpoint, "api/v2/secdump"),
		})
	}

	return storage, nil
}

// GetStorageType returns the storage type of the ActivityDumpLocalStorage
func (storage *ActivityDumpRemoteStorage) GetStorageType() config.StorageType {
	return config.RemoteStorage
}

func (storage *ActivityDumpRemoteStorage) writeEventMetadata(writer *multipart.Writer, ad *ActivityDump) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="event"; filename=""`)
	h.Set("Content-Type", "application/json")

	dataWriter, err := writer.CreatePart(h)
	if err != nil {
		return fmt.Errorf("couldn't create event metadata part: %w", err)
	}

	// prepare tags for serialisation
	ad.DDTags = strings.Join(ad.Tags, ",")

	// marshal event metadata
	metadata, err := json.Marshal(ad.ActivityDumpHeader)
	if err != nil {
		return fmt.Errorf("couldn't marshall event metadata")
	}

	// write metadata
	if _, err = dataWriter.Write(metadata); err != nil {
		return fmt.Errorf("couldn't write event metadata part: %w", err)
	}
	return err
}

func (storage *ActivityDumpRemoteStorage) writeDump(writer *multipart.Writer, request config.StorageRequest, raw *bytes.Buffer) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="dump"; filename="dump.%s"`, request.Format.String()))
	h.Set("Content-Type", "application/json")

	dataWriter, err := writer.CreatePart(h)
	if err != nil {
		return fmt.Errorf("couldn't create dump part: %w", err)
	}
	if _, err = dataWriter.Write(raw.Bytes()); err != nil {
		return fmt.Errorf("couldn't write dump part: %w", err)
	}
	return nil
}

func (storage *ActivityDumpRemoteStorage) buildBody(request config.StorageRequest, ad *ActivityDump, raw *bytes.Buffer) (*multipart.Writer, *bytes.Buffer, error) {
	body := bytes.NewBuffer(nil)
	var multipartWriter *multipart.Writer

	if request.Compression {
		compressor := gzip.NewWriter(body)
		defer compressor.Close()
		multipartWriter = multipart.NewWriter(compressor)
	} else {
		multipartWriter = multipart.NewWriter(body)
	}
	defer multipartWriter.Close()

	// set activity dump size
	ad.Metadata.Size = uint64(len(raw.Bytes()))

	if err := storage.writeEventMetadata(multipartWriter, ad); err != nil {
		return nil, nil, err
	}

	if err := storage.writeDump(multipartWriter, request, raw); err != nil {
		return nil, nil, err
	}
	return multipartWriter, body, nil
}

func (storage *ActivityDumpRemoteStorage) sendToEndpoint(url string, apiKey string, request config.StorageRequest, writer *multipart.Writer, body *bytes.Buffer) error {
	r, err := http.NewRequest("POST", url, bytes.NewBuffer(body.Bytes()))
	if err != nil {
		return err
	}
	r.Header.Add("Content-Type", writer.FormDataContentType())
	r.Header.Add("dd-api-key", apiKey)

	if request.Compression {
		r.Header.Set("Content-Encoding", "gzip")
	}

	resp, err := storage.client.Do(r)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		entry := tooLargeEntityStatsEntry{
			storageFormat: request.Format,
			compression:   request.Compression,
		}
		storage.tooLargeEntities[entry].Inc()
	}
	return errors.New(resp.Status)
}

// Persist saves the provided buffer to the persistent storage
func (storage *ActivityDumpRemoteStorage) Persist(request config.StorageRequest, ad *ActivityDump, raw *bytes.Buffer) error {
	writer, body, err := storage.buildBody(request, ad, raw)
	if err != nil {
		return fmt.Errorf("couldn't build request: %w", err)
	}

	for _, endpoint := range storage.endpoints {
		if err := storage.sendToEndpoint(endpoint.url, endpoint.logsEndpoint.GetAPIKey(), request, writer, body); err != nil {
			seclog.Warnf("couldn't sent activity dump to [%s, body size: %d, dump size: %d]: %v", endpoint.url, body.Len(), ad.Size, err)
		} else {
			seclog.Infof("[%s] file for activity dump [%s] successfully sent to [%s]", request.Format, ad.GetSelectorStr(), endpoint.url)
		}
	}

	return nil
}

// SendTelemetry sends telemetry for the current storage
func (storage *ActivityDumpRemoteStorage) SendTelemetry(sender statsd.ClientInterface) {
	// send too large entity metric
	for entry, count := range storage.tooLargeEntities {
		if entityCount := count.Swap(0); entityCount > 0 {
			tags := []string{fmt.Sprintf("format:%s", entry.storageFormat.String()), fmt.Sprintf("compression:%v", entry.compression)}
			_ = sender.Count(metrics.MetricActivityDumpEntityTooLarge, int64(entityCount), tags, 1.0)
		}
	}
}
