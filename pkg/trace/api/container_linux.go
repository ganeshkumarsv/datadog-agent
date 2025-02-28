// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

//go:build !serverless

package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/datadog-agent/comp/core/tagger/origindetection"
	"github.com/DataDog/datadog-agent/pkg/trace/api/internal/header"
	"github.com/DataDog/datadog-agent/pkg/util/cgroups"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// cgroupV1BaseController is the name of the cgroup controller used to parse /proc/<pid>/cgroup
const cgroupV1BaseController = "memory"

// readerCacheExpiration determines the duration for which the cgroups data is cached in the cgroups reader.
// This value needs to be large enough to reduce latency and I/O load.
// It also needs to be small enough to catch the first traces of new containers.
const readerCacheExpiration = 2 * time.Second

type ucredKey struct{}

// connContext injects a Unix Domain Socket's User Credentials into the
// context.Context object provided. This is useful as the connContext member of an http.Server, to
// provide User Credentials to HTTP handlers.
//
// If the connection c is not a *net.UnixConn, the unchanged context is returned.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if oc, ok := c.(*onCloseConn); ok {
		c = oc.Conn
	}
	s, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, err := s.SyscallConn()
	if err != nil {
		log.Debugf("Failed to read credentials from unix socket: %v", err)
		return ctx
	}
	var (
		ucred *syscall.Ucred
		cerr  error
	)
	err = raw.Control(func(fd uintptr) {
		ucred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		log.Debugf("Failed to control raw unix socket: %v", err)
		return ctx
	}
	if cerr != nil {
		log.Debugf("Failed to read credentials from unix socket: %v", cerr)
		return ctx
	}

	return context.WithValue(ctx, ucredKey{}, ucred)
}

// cacheExpiration determines how long a pid->container ID mapping is considered valid. This value is
// somewhat arbitrarily chosen, but just needs to be large enough to reduce latency and I/O load
// caused by frequently reading mappings, and small enough that pid-reuse doesn't cause mismatching
// of pids with container ids. A one minute cache means the latency and I/O should be low, and
// there would have to be thousands of containers spawned and dying per second to cause a mismatch.
const cacheExpiration = time.Minute

// IDProvider implementations are able to look up a container ID given a ctx and http header.
type IDProvider interface {
	GetContainerID(context.Context, http.Header) string
}

// noCgroupsProvider is a fallback IDProvider that only looks in the http header for a container ID.
type noCgroupsProvider struct{}

func (i *noCgroupsProvider) GetContainerID(_ context.Context, h http.Header) string {
	return h.Get(header.ContainerID)
}

// NewIDProvider initializes an IDProvider instance using the provided procRoot to perform cgroups lookups in linux environments.
func NewIDProvider(procRoot string, containerIDFromOriginInfo func(originInfo origindetection.OriginInfo) (string, error)) IDProvider {
	// taken from pkg/util/containers/metrics/system.collector_linux.go
	var hostPrefix string
	if strings.HasPrefix(procRoot, "/host") {
		hostPrefix = "/host"
	}

	reader, err := cgroups.NewReader(
		cgroups.WithCgroupV1BaseController(cgroupV1BaseController),
		cgroups.WithProcPath(procRoot),
		cgroups.WithHostPrefix(hostPrefix),
		cgroups.WithReaderFilter(cgroups.ContainerFilter), // Will parse the path in /proc/<pid>/cgroup to get the container ID.
	)

	if err != nil {
		log.Warnf("Failed to identify cgroups version due to err: %v. APM data may be missing containerIDs for applications running in containers. This will prevent spans from being associated with container tags.", err)
		return &noCgroupsProvider{}
	}
	cgroupController := ""
	if reader.CgroupVersion() == 1 {
		cgroupController = cgroupV1BaseController // The 'memory' controller is used by the cgroupv1 utils in the agent to parse the procfs.
	}
	c := NewCache(1 * time.Minute)
	return &cgroupIDProvider{
		procRoot:                  procRoot,
		controller:                cgroupController,
		cache:                     c,
		reader:                    reader,
		containerIDFromOriginInfo: containerIDFromOriginInfo,
	}
}

type cgroupIDProvider struct {
	procRoot   string
	controller string
	// reader is used to retrieve the container ID from its cgroup v2 inode.
	reader                    *cgroups.Reader
	cache                     *Cache
	containerIDFromOriginInfo func(originInfo origindetection.OriginInfo) (string, error)
}

// GetContainerID returns the container ID.
// The Container ID can come from either http headers or the context:
// * Local Data header
// * Datadog-Container-ID header
// * Looks for a PID in the ctx which is used to search cgroups for a container ID.
func (c *cgroupIDProvider) GetContainerID(ctx context.Context, h http.Header) string {
	originInfo := origindetection.OriginInfo{ProductOrigin: origindetection.ProductOriginAPM}

	// Retrieve container ID from Local Data header
	if localDataString := h.Get(header.LocalData); localDataString != "" {
		var err error
		originInfo.LocalData, err = origindetection.ParseLocalData(localDataString)
		if err != nil {
			log.Errorf("Could not parse local data (%s): %v", localDataString, err)
		}

		if originInfo.LocalData.ContainerID != "" {
			return originInfo.LocalData.ContainerID
		} else if originInfo.LocalData.Inode != 0 {
			return c.resolveContainerIDFromInode(strconv.FormatUint(originInfo.LocalData.Inode, 10))
		}
	}

	// Retrieve container ID from Datadog-Container-ID header.
	// Deprecated in favor of Local Data header. This is kept for backward compatibility with older libraries.
	if containerIDFromHeader := h.Get(header.ContainerID); containerIDFromHeader != "" {
		return containerIDFromHeader
	}

	// Retrieve the container-id from the pid in its context
	if containerID := c.resolveContainerIDFromContext(ctx); containerID != "" {
		return containerID
	}

	// Retrieve container ID from External Data header
	if externalData := h.Get(header.ExternalData); externalData != "" {
		return c.resolveContainerIDFromExternalData(externalData)
	}

	return ""
}

// resolveContainerIDFromInode returns the container ID for the given cgroupv2 inode.
func (c *cgroupIDProvider) resolveContainerIDFromInode(inodeString string) string {
	containerID, err := c.getCachedContainerID(inodeString, func() (string, error) {
		// Parse the cgroupv2 inode as a uint64.
		inode, err := strconv.ParseUint(inodeString, 10, 64)
		if err != nil {
			return "", fmt.Errorf("could not parse cgroupv2 inode: %s: %v", inodeString, err)
		}

		// Get the container ID from the cgroupv2 inode.
		cgroup := c.reader.GetCgroupByInode(inode)
		if cgroup == nil {
			err := c.reader.RefreshCgroups(readerCacheExpiration)
			if err != nil {
				return "", fmt.Errorf("containerID not found from inode %d and unable to refresh cgroups, err: %w", inode, err)
			}

			cgroup = c.reader.GetCgroupByInode(inode)
			if cgroup == nil {
				return "", fmt.Errorf("containerID not found from inode %d, err: %w", inode, err)
			}
		}

		return cgroup.Identifier(), nil
	})
	if err != nil {
		log.Debugf("Could not get container ID from cgroupv2 inode: %s: %v", inodeString, err)
		return ""
	}

	return containerID
}

// resolveContainerIDFromContext returns the container ID for the given context.
// This is a fallback for when the container ID is not available in the http headers.
func (c *cgroupIDProvider) resolveContainerIDFromContext(ctx context.Context) string {
	ucred, ok := ctx.Value(ucredKey{}).(*syscall.Ucred)
	if !ok || ucred == nil {
		return ""
	}
	pid := strconv.Itoa(int(ucred.Pid))
	cid, err := c.getCachedContainerID(
		pid,
		func() (string, error) {
			return cgroups.IdentiferFromCgroupReferences(c.procRoot, pid, c.controller, cgroups.ContainerFilter)
		},
	)
	if err != nil {
		log.Debugf("Could not get container ID from pid: %d: %v\n", ucred.Pid, err)
		return ""
	}
	return cid
}

// getCachedContainerID returns the container ID for the given key, using a cache.
func (c *cgroupIDProvider) getCachedContainerID(key string, retrievalFunc func() (string, error)) (string, error) {
	currentTime := time.Now()
	entry, found, err := c.cache.Get(currentTime, key, cacheExpiration)
	if found {
		if err != nil {
			return "", err
		}

		return entry.(string), nil
	}

	// No cache, cacheValidity is 0 or too old value
	val, err := retrievalFunc()
	if err != nil {
		c.cache.Store(currentTime, key, nil, err)
		return "", err
	}

	c.cache.Store(currentTime, key, val, nil)
	return val, nil
}

// resolveContainerIDFromExternalData returns the container ID for the given External Data.
func (c *cgroupIDProvider) resolveContainerIDFromExternalData(rawExternalData string) string {
	var generatedContainerID string

	externalData, err := origindetection.ParseExternalData(rawExternalData)
	if err != nil {
		log.Errorf("Could not parse external data (%s): %v", rawExternalData, err)
		return ""
	}
	generatedContainerID, err = c.containerIDFromOriginInfo(origindetection.OriginInfo{
		ExternalData:  externalData,
		ProductOrigin: origindetection.ProductOriginAPM,
	})
	if err != nil {
		log.Errorf("Could not generate container ID from external data (%s): %v", rawExternalData, err)
		return ""
	}

	return generatedContainerID
}

// The below cache is copied from /pkg/util/containers/v2/metrics/provider/cache.go. It is not
// imported to avoid making the datadog-agent module a dependency of the pkg/trace module. The
// datadog-agent module contains replace directives which are not inherited by packages that
// require it, and cannot be guaranteed to function correctly as a dependency.
type cacheEntry struct {
	value     interface{}
	err       error
	timestamp time.Time
}

// Cache provides a caching mechanism based on staleness toleration provided by requestor
type Cache struct {
	cache       map[string]cacheEntry
	cacheLock   sync.RWMutex
	gcInterval  time.Duration
	gcTimestamp time.Time
}

// NewCache returns a new cache dedicated to a collector
func NewCache(gcInterval time.Duration) *Cache {
	return &Cache{
		cache:      make(map[string]cacheEntry),
		gcInterval: gcInterval,
	}
}

// Get retrieves data from cache, returns not found if cacheValidity == 0
func (c *Cache) Get(currentTime time.Time, key string, cacheValidity time.Duration) (interface{}, bool, error) {
	if cacheValidity <= 0 {
		return nil, false, nil
	}

	c.cacheLock.RLock()
	entry, found := c.cache[key]
	c.cacheLock.RUnlock()

	if !found || currentTime.Sub(entry.timestamp) > cacheValidity {
		return nil, false, nil
	}

	if entry.err != nil {
		return nil, true, entry.err
	}

	return entry.value, true, nil
}

// Store sets data in the cache, it also clears the cache if the gcInterval has passed
func (c *Cache) Store(currentTime time.Time, key string, value interface{}, err error) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	if currentTime.Sub(c.gcTimestamp) > c.gcInterval {
		c.cache = make(map[string]cacheEntry, len(c.cache))
		c.gcTimestamp = currentTime
	}

	c.cache[key] = cacheEntry{value: value, timestamp: currentTime, err: err}
}
