// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (c) 2024-2026 usulnet contributors
// https://github.com/fr4nsys/usulnet

package update

import (
	"context"
	"sync"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// fakeDockerClient implements DockerClient for waitForHealthy tests. Only
// ContainerInspect is meaningful; inspect returns the state for the current
// call index (the last entry repeats once the sequence is exhausted).
type fakeDockerClient struct {
	mu     sync.Mutex
	calls  int
	states []*dockertypes.ContainerJSON
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, _ string) (*dockertypes.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	idx := f.calls - 1
	if idx >= len(f.states) {
		idx = len(f.states) - 1
	}
	return f.states[idx], nil
}

func (f *fakeDockerClient) ContainerStop(context.Context, string, *int) error       { return nil }
func (f *fakeDockerClient) ContainerStart(context.Context, string) error            { return nil }
func (f *fakeDockerClient) ContainerRemove(context.Context, string, bool) error     { return nil }
func (f *fakeDockerClient) ContainerRename(context.Context, string, string) error   { return nil }
func (f *fakeDockerClient) ContainerList(context.Context) ([]ContainerInfo, error) { return nil, nil }
func (f *fakeDockerClient) ImagePull(context.Context, string, func(string)) error   { return nil }
func (f *fakeDockerClient) ImageInspect(context.Context, string) (*ImageInfo, error) {
	return nil, nil
}
func (f *fakeDockerClient) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, string) (string, error) {
	return "", nil
}

func inspectWith(hc *container.HealthConfig, running, restarting bool, health string) *dockertypes.ContainerJSON {
	info := &dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			State: &dockertypes.ContainerState{Running: running, Restarting: restarting},
		},
		Config: &container.Config{Healthcheck: hc},
	}
	if health != "" {
		info.State.Health = &dockertypes.Health{Status: health}
	}
	return info
}

func TestHealthcheckDuration(t *testing.T) {
	if got := healthcheckDuration(nil); got != 0 {
		t.Errorf("nil info: got %v, want 0", got)
	}
	if got := healthcheckDuration(inspectWith(nil, true, false, "")); got != 0 {
		t.Errorf("no healthcheck: got %v, want 0", got)
	}

	// guacd-style healthcheck: start 10s, interval 30s, timeout 5s, 3 retries.
	hc := &container.HealthConfig{
		StartPeriod: 10 * time.Second,
		Interval:    30 * time.Second,
		Timeout:     5 * time.Second,
		Retries:     3,
	}
	want := 10*time.Second + 4*(35*time.Second)
	if got := healthcheckDuration(inspectWith(hc, true, false, "starting")); got != want {
		t.Errorf("guacd-style: got %v, want %v", got, want)
	}

	// Zero fields fall back to Docker's defaults instead of producing 0.
	if got := healthcheckDuration(inspectWith(&container.HealthConfig{}, true, false, "")); got <= 0 {
		t.Errorf("empty healthcheck: got %v, want > 0", got)
	}
}

func TestWaitForHealthy_WaitsPastConfiguredWaitForStartingContainer(t *testing.T) {
	// Healthcheck needs (3+1)*(500ms+100ms) = 2.4s; the configured wait is
	// only 100ms. The container reports "starting" on the first poll and
	// "healthy" on the second, which the old fixed wait would have missed.
	hc := &container.HealthConfig{Interval: 500 * time.Millisecond, Timeout: 100 * time.Millisecond, Retries: 3}
	fake := &fakeDockerClient{states: []*dockertypes.ContainerJSON{
		inspectWith(hc, true, false, "starting"), // deadline probe
		inspectWith(hc, true, false, "starting"), // poll at ~0.8s
		inspectWith(hc, true, false, "healthy"),  // poll at ~1.6s

	}}
	svc := &Service{dockerClient: fake}

	start := time.Now()
	ok := svc.waitForHealthy(context.Background(), "c1", 100*time.Millisecond, 3)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("expected healthy, got false after %v", elapsed)
	}
	if elapsed < 1*time.Second {
		t.Errorf("returned after %v; expected to keep polling past the 100ms configured wait", elapsed)
	}
	if elapsed > 2200*time.Millisecond {
		t.Errorf("returned after %v; expected to stop as soon as healthy", elapsed)
	}
}

func TestWaitForHealthy_FailsFastOnUnhealthyOrExited(t *testing.T) {
	hc := &container.HealthConfig{Interval: time.Second, Timeout: 100 * time.Millisecond, Retries: 3}

	unhealthy := &fakeDockerClient{states: []*dockertypes.ContainerJSON{
		inspectWith(hc, true, false, "starting"),
		inspectWith(hc, true, false, "unhealthy"),
	}}
	start := time.Now()
	if ok := (&Service{dockerClient: unhealthy}).waitForHealthy(context.Background(), "c1", time.Second, 3); ok {
		t.Error("unhealthy container reported healthy")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("unhealthy verdict took %v; expected fail-fast", time.Since(start))
	}

	exited := &fakeDockerClient{states: []*dockertypes.ContainerJSON{
		inspectWith(nil, true, false, ""),
		inspectWith(nil, false, false, ""),
	}}
	if ok := (&Service{dockerClient: exited}).waitForHealthy(context.Background(), "c1", time.Second, 3); ok {
		t.Error("exited container reported healthy")
	}
}

func TestWaitForHealthy_NoHealthcheckRunningIsHealthy(t *testing.T) {
	fake := &fakeDockerClient{states: []*dockertypes.ContainerJSON{inspectWith(nil, true, false, "")}}
	if ok := (&Service{dockerClient: fake}).waitForHealthy(context.Background(), "c1", time.Second, 3); !ok {
		t.Error("running container without healthcheck reported unhealthy")
	}
}

func TestBuildNetworkingConfig(t *testing.T) {
	if got := buildNetworkingConfig(nil); got != nil {
		t.Errorf("nil info: got %+v, want nil", got)
	}

	const id = "0123456789abcdef0123456789abcdef"
	info := &dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{ID: id},
		NetworkSettings: &dockertypes.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"usulnet_frontend": {
					Aliases:    []string{"guacd", id[:12]},
					IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: "10.0.0.5"},
				},
				"usulnet_backend": {Aliases: []string{"guacd"}},
				"bridge":          nil,
			},
		},
	}

	got := buildNetworkingConfig(info)
	if got == nil || len(got.EndpointsConfig) != 3 {
		t.Fatalf("expected 3 endpoints, got %+v", got)
	}
	fe := got.EndpointsConfig["usulnet_frontend"]
	if len(fe.Aliases) != 1 || fe.Aliases[0] != "guacd" {
		t.Errorf("frontend aliases = %v, want [guacd] (short-ID alias dropped)", fe.Aliases)
	}
	if fe.IPAMConfig == nil || fe.IPAMConfig.IPv4Address != "10.0.0.5" {
		t.Errorf("frontend IPAM config not preserved: %+v", fe.IPAMConfig)
	}
	if be := got.EndpointsConfig["usulnet_backend"]; len(be.Aliases) != 1 {
		t.Errorf("backend aliases = %v, want [guacd]", be.Aliases)
	}
	if got.EndpointsConfig["bridge"] == nil {
		t.Error("nil endpoint should produce an empty endpoint, not be dropped")
	}
}

func TestSplitEndpoints(t *testing.T) {
	if p, extra := splitEndpoints(nil, nil); p != nil || extra != nil {
		t.Errorf("nil config: got %+v / %+v, want nil / nil", p, extra)
	}

	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		"zeta":  {Aliases: []string{"z"}},
		"alpha": {Aliases: []string{"a"}},
		"mid":   {Aliases: []string{"m"}},
	}}

	// NetworkMode names an attached network: it becomes the primary.
	primary, extra := splitEndpoints(nc, &container.HostConfig{NetworkMode: "mid"})
	if len(primary.EndpointsConfig) != 1 || primary.EndpointsConfig["mid"] == nil {
		t.Errorf("primary = %+v, want only mid", primary.EndpointsConfig)
	}
	if len(extra) != 2 || extra["zeta"] == nil || extra["alpha"] == nil {
		t.Errorf("extra = %+v, want zeta and alpha", extra)
	}

	// NetworkMode not among the attachments: first by name is primary.
	primary, extra = splitEndpoints(nc, &container.HostConfig{NetworkMode: "host"})
	if primary.EndpointsConfig["alpha"] == nil || len(extra) != 2 {
		t.Errorf("fallback primary = %+v, extra = %+v; want alpha primary", primary.EndpointsConfig, extra)
	}
}
